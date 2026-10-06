// Package gwfake is an in-memory stand-in for one graywolf station's
// Messages API, for tests. It is written from graywolf's published API
// and observed behaviour (spec section 0), not from graywolf source.
//
// Modelled behaviour: graywolf assigns row ids and numeric msgids;
// resend keeps the msgid and does NOT reset an acked/rejected status;
// a resend while one is in flight is a 409; an unknown row is a 404.
package gwfake

import (
	"cmp"
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
)

// Transmission is one frame the station put on air.
type Transmission struct {
	ID     uint64
	To     string
	Text   string
	Resend bool
}

// Station fakes one graywolf's Messages API.
type Station struct {
	Call string
	// EchoClientID makes rows remember the client_id they were sent with.
	EchoClientID bool
	// NoAckedAt leaves acked_at unset on Ack/Reject.
	NoAckedAt bool
	Now       func() time.Time

	mu         sync.Mutex
	nextID     uint64
	rows       map[uint64]*graywolf.Message
	order      []uint64
	tx         []Transmission
	inFlight   map[uint64]bool
	sendErrs   []error
	resendErrs []error
	prefs      map[string]graywolf.ConversationPrefs
	prefsErr   error
}

// New returns a station with the given callsign.
func New(call string) *Station {
	return &Station{Call: call, Now: time.Now, rows: map[uint64]*graywolf.Message{}, inFlight: map[uint64]bool{},
		prefs: map[string]graywolf.ConversationPrefs{}}
}

func apiErr(code int, msg string) error {
	return &graywolf.APIError{Method: "POST", Path: "/api/messages", StatusCode: code, Message: msg}
}

// FailNextSend makes the next SendMessage calls fail with errs, in order.
func (s *Station) FailNextSend(errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendErrs = append(s.sendErrs, errs...)
}

// FailNextResend makes the next ResendMessage calls fail with errs, in order.
func (s *Station) FailNextResend(errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resendErrs = append(s.resendErrs, errs...)
}

// SetInFlight marks row id as having a send in progress (resend → 409).
func (s *Station) SetInFlight(id uint64, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[id] = on
}

func (s *Station) insert(m graywolf.Message) graywolf.Message {
	s.nextID++
	m.ID = s.nextID
	now := s.Now().UTC()
	m.CreatedAt = &now
	s.rows[m.ID] = &m
	s.order = append(s.order, m.ID)
	return m
}

// SendMessage implements graywolf.Client.SendMessage.
func (s *Station) SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sendErrs) > 0 {
		err := s.sendErrs[0]
		s.sendErrs = s.sendErrs[1:]
		return graywolf.Message{}, err
	}
	to := strings.ToUpper(req.To)
	m := graywolf.Message{
		Direction: "out", ThreadKind: graywolf.ThreadKindDM, FromCall: s.Call, ToCall: to, PeerCall: to,
		Text: req.Text, Path: req.Path, Status: graywolf.StatusSentRF, Attempts: 1,
	}
	if s.EchoClientID {
		m.ClientID = req.ClientID
	}
	m = s.insert(m)
	s.rows[m.ID].MsgID = strconv.FormatUint(m.ID, 10)
	sent := s.Now().UTC()
	s.rows[m.ID].SentAt = &sent
	s.tx = append(s.tx, Transmission{ID: m.ID, To: to, Text: req.Text})
	out := *s.rows[m.ID]
	out.ClientID = req.ClientID // the POST response always echoes it
	return out, nil
}

// ResendMessage implements graywolf.Client.ResendMessage.
func (s *Station) ResendMessage(ctx context.Context, id uint64) (graywolf.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.resendErrs) > 0 {
		err := s.resendErrs[0]
		s.resendErrs = s.resendErrs[1:]
		return graywolf.Message{}, err
	}
	m, ok := s.rows[id]
	if !ok {
		return graywolf.Message{}, apiErr(http.StatusNotFound, "message not found")
	}
	if s.inFlight[id] {
		return graywolf.Message{}, apiErr(http.StatusConflict, "resend already in flight")
	}
	m.Attempts++ // status deliberately unchanged
	s.tx = append(s.tx, Transmission{ID: id, To: m.ToCall, Text: m.Text, Resend: true})
	return *m, nil
}

// GetMessage implements graywolf.Client.GetMessage.
func (s *Station) GetMessage(ctx context.Context, id uint64) (graywolf.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.rows[id]
	if !ok {
		return graywolf.Message{}, apiErr(http.StatusNotFound, "message not found")
	}
	return *m, nil
}

// CatchUp implements a single-page graywolf.Client.CatchUp over rows in
// id order, honouring Folder, Peer and Since. The cursor is the last id.
func (s *Station) CatchUp(ctx context.Context, p graywolf.ListParams, fn func(graywolf.MessageChange) error) (string, error) {
	s.mu.Lock()
	var page []graywolf.Message
	after, _ := strconv.ParseUint(p.Cursor, 10, 64)
	for _, id := range s.order {
		m, ok := s.rows[id]
		if !ok || id <= after || !matches(*m, p) {
			continue
		}
		page = append(page, *m)
	}
	s.mu.Unlock()
	cursor := p.Cursor
	for _, m := range page {
		if err := fn(graywolf.MessageChange{ID: m.ID, Kind: "created", Message: &m}); err != nil {
			return cursor, err
		}
		cursor = strconv.FormatUint(m.ID, 10)
	}
	return cursor, nil
}

func matches(m graywolf.Message, p graywolf.ListParams) bool {
	switch p.Folder {
	case graywolf.FolderSent:
		if m.Direction != "out" {
			return false
		}
	case graywolf.FolderInbox:
		if m.Direction != "in" {
			return false
		}
	}
	if p.Peer != "" && !strings.EqualFold(m.PeerCall, p.Peer) {
		return false
	}
	return p.Since.IsZero() || m.CreatedAt == nil || !m.CreatedAt.Before(p.Since)
}

// Ack marks row id acked by its peer, as graywolf does on a matching ACK.
func (s *Station) Ack(id uint64) { s.setStatus(id, graywolf.StatusAcked) }

// Reject marks row id rejected by its peer.
func (s *Station) Reject(id uint64) { s.setStatus(id, graywolf.StatusRejected) }

func (s *Station) setStatus(id uint64, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.rows[id]; ok {
		m.Status = status
		if !s.NoAckedAt {
			now := s.Now().UTC()
			m.AckedAt = &now
		}
	}
}

// Delete removes row id (an operator deleting it in graywolf's UI).
func (s *Station) Delete(id uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.rows, id)
}

// Inbound adds a DM from `from` to this station, as if heard on air.
func (s *Station) Inbound(from, text string) graywolf.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.insert(graywolf.Message{
		Direction: "in", ThreadKind: graywolf.ThreadKindDM, FromCall: from, ToCall: s.Call, PeerCall: from,
		Text: text, Status: graywolf.StatusReceived, Unread: true,
	})
}

// Transmissions returns every frame put on air, in order.
func (s *Station) Transmissions() []Transmission {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.tx)
}

// TransmissionsWithPrefix returns frames whose text starts with prefix.
func (s *Station) TransmissionsWithPrefix(prefix string) []Transmission {
	var out []Transmission
	for _, t := range s.Transmissions() {
		if strings.HasPrefix(t.Text, prefix) {
			out = append(out, t)
		}
	}
	return out
}

// Rows returns every row, by id.
func (s *Station) Rows() []graywolf.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]graywolf.Message, 0, len(s.rows))
	for _, m := range s.rows {
		out = append(out, *m)
	}
	slices.SortFunc(out, func(a, b graywolf.Message) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// FailPrefs makes conversation-prefs calls fail with err (nil clears it).
func (s *Station) FailPrefs(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prefsErr = err
}

// ConversationPrefs implements graywolf.Client.ConversationPrefs: no
// stored override reads as the defaults (inherit, wait_for_ack=true).
func (s *Station) ConversationPrefs(ctx context.Context, kind, key string) (graywolf.ConversationPrefs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefsErr != nil {
		return graywolf.ConversationPrefs{}, s.prefsErr
	}
	p, ok := s.prefs[kind+"/"+strings.ToUpper(key)]
	if !ok {
		return graywolf.ConversationPrefs{ThreadKind: kind, ThreadKey: strings.ToUpper(key), WaitForAck: true}, nil
	}
	return p, nil
}

// SetConversationPrefs implements graywolf.Client.SetConversationPrefs.
// Like graywolf, storing the defaults removes the override.
func (s *Station) SetConversationPrefs(ctx context.Context, kind, key string, p graywolf.ConversationPrefs) (graywolf.ConversationPrefs, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefsErr != nil {
		return graywolf.ConversationPrefs{}, s.prefsErr
	}
	k := kind + "/" + strings.ToUpper(key)
	out := graywolf.ConversationPrefs{ThreadKind: kind, ThreadKey: strings.ToUpper(key), SendPath: p.SendPath, WaitForAck: p.WaitForAck}
	if p.SendPath == "" && p.WaitForAck {
		delete(s.prefs, k)
	} else {
		s.prefs[k] = out
	}
	return out, nil
}

// HasPrefsOverride reports whether a non-default override is stored.
func (s *Station) HasPrefsOverride(kind, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.prefs[kind+"/"+strings.ToUpper(key)]
	return ok
}
