package radiobackend

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

// SendMessage queues a DM for the radio, as graywolf's POST /api/messages
// does. The row goes into the radio's queue when it has room.
func (b *Backend) SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error) {
	call, ssid, err := uvk5.SplitCallsign(req.To)
	if err != nil {
		return graywolf.Message{}, apiErr(http.StatusBadRequest, "/api/messages", err.Error())
	}
	if err := uvk5.ValidText(req.Text); err != nil {
		return graywolf.Message{}, apiErr(http.StatusBadRequest, "/api/messages", err.Error())
	}
	to := call
	if ssid > 0 {
		to += "-" + strconv.Itoa(int(ssid))
	}
	own := b.ownCall()
	row := &store.RadioMessage{Direction: "out", Status: graywolf.StatusQueued, FromCall: own, ToCall: to, PeerCall: to,
		Text: req.Text, ClientID: req.ClientID, CreatedAt: b.now()}
	if err := b.st.RadioInsert(ctx, row); err != nil {
		return graywolf.Message{}, err
	}
	b.notify(row.ID, graywolf.EventUpdated)
	b.kick()
	return b.toMessage(*row), nil
}

// ResendMessage sends a failed or rejected row again (a new line number
// on air). Like graywolf with its retries off, a row still pending or
// already acked is refused with 409.
func (b *Backend) ResendMessage(ctx context.Context, id uint64) (graywolf.Message, error) {
	path := "/api/messages/" + strconv.FormatUint(id, 10) + "/resend"
	var refuse error
	row, err := b.st.RadioUpdate(ctx, id, func(m *store.RadioMessage) bool {
		switch {
		case m.Direction != "out":
			refuse = apiErr(http.StatusBadRequest, path, "not an outbound message")
		case m.Status == graywolf.StatusAcked:
			refuse = apiErr(http.StatusConflict, path, `message is already in terminal state "acked"`)
		case m.Status != graywolf.StatusFailed && m.Status != graywolf.StatusRejected:
			refuse = apiErr(http.StatusConflict, path, "message is still pending; cannot resend until it fails or is acked")
		default:
			m.Status, m.MsgID, m.FailureReason, m.SentAt = graywolf.StatusQueued, "", "", nil
			return true
		}
		return false
	})
	if errors.Is(err, store.ErrNotFound) {
		return graywolf.Message{}, apiErr(http.StatusNotFound, path, "message not found")
	}
	if err != nil {
		return graywolf.Message{}, err
	}
	if refuse != nil {
		return graywolf.Message{}, refuse
	}
	b.notify(id, graywolf.EventUpdated)
	b.kick()
	return b.toMessage(row), nil
}

// GetMessage returns row id.
func (b *Backend) GetMessage(ctx context.Context, id uint64) (graywolf.Message, error) {
	row, err := b.st.RadioGet(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return graywolf.Message{}, apiErr(http.StatusNotFound, "/api/messages/"+strconv.FormatUint(id, 10), "message not found")
	}
	if err != nil {
		return graywolf.Message{}, err
	}
	return b.toMessage(row), nil
}

// CatchUp delivers rows changed since p.Cursor, in change order,
// honouring Folder, Peer and Since; the cursor is the last change seen.
func (b *Backend) CatchUp(ctx context.Context, p graywolf.ListParams, fn func(graywolf.MessageChange) error) (string, error) {
	after, _ := strconv.ParseUint(p.Cursor, 10, 64)
	rows, err := b.st.RadioChanges(ctx, after, 0)
	if err != nil {
		return p.Cursor, err
	}
	cursor := p.Cursor
	for _, row := range rows {
		if matches(row, p) {
			m := b.toMessage(row)
			if err := fn(graywolf.MessageChange{ID: m.ID, Kind: "updated", Message: &m}); err != nil {
				return cursor, err
			}
		}
		cursor = strconv.FormatUint(row.UpdSeq, 10)
	}
	return cursor, nil
}

func matches(m store.RadioMessage, p graywolf.ListParams) bool {
	switch p.Folder {
	case graywolf.FolderInbox:
		if m.Direction != "in" {
			return false
		}
	case graywolf.FolderSent:
		if m.Direction != "out" {
			return false
		}
	}
	if p.Peer != "" && !strings.EqualFold(p.Peer, m.PeerCall) {
		return false
	}
	if p.UnreadOnly && !m.Unread {
		return false
	}
	return p.Since.IsZero() || !m.CreatedAt.Before(p.Since)
}

// StreamEventsWithOpen calls onOpen, then delivers a hint for every row
// change until ctx ends.
func (b *Backend) StreamEventsWithOpen(ctx context.Context, onOpen func(), fn func(graywolf.Event) error) error {
	ch := make(chan graywolf.Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}()
	if onOpen != nil {
		onOpen()
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev := <-ch:
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
}

// MarkRead clears row id's unread flag.
func (b *Backend) MarkRead(ctx context.Context, id uint64) error {
	_, err := b.st.RadioUpdate(ctx, id, func(m *store.RadioMessage) bool {
		if !m.Unread {
			return false
		}
		m.Unread = false
		return true
	})
	if errors.Is(err, store.ErrNotFound) {
		return apiErr(http.StatusNotFound, "/api/messages/"+strconv.FormatUint(id, 10)+"/read", "message not found")
	}
	if err == nil {
		b.notify(id, graywolf.EventUpdated)
	}
	return err
}

// DeleteMessage removes row id (post-race cleanup).
func (b *Backend) DeleteMessage(ctx context.Context, id uint64) error {
	err := b.st.RadioDelete(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return apiErr(http.StatusNotFound, "/api/messages/"+strconv.FormatUint(id, 10), "message not found")
	}
	if err == nil {
		b.notify(id, graywolf.EventDeleted)
	}
	return err
}

// ConversationPrefs: the radio never retries, so it's always "no wait
// for ack" (what the engines want of graywolf).
func (b *Backend) ConversationPrefs(ctx context.Context, kind, key string) (graywolf.ConversationPrefs, error) {
	return graywolf.ConversationPrefs{ThreadKind: kind, ThreadKey: key}, nil
}

// SetConversationPrefs accepts and ignores per-peer settings: there are
// none on the radio.
func (b *Backend) SetConversationPrefs(ctx context.Context, kind, key string, p graywolf.ConversationPrefs) (graywolf.ConversationPrefs, error) {
	p.ThreadKind, p.ThreadKey, p.WaitForAck = kind, key, false
	return p, nil
}

// MessagePreferences reports the radio's limits: 67-character messages,
// no retries.
func (b *Backend) MessagePreferences(ctx context.Context) (graywolf.MessagePreferences, error) {
	return graywolf.MessagePreferences{}, nil
}

// ListPackets returns messages the radio heard (RX), newest last, as
// graywolf's packet log does. There's no audio level.
func (b *Backend) ListPackets(ctx context.Context, q graywolf.PacketQuery) ([]graywolf.Packet, error) {
	b.mu.Lock()
	all := slices.Clone(b.packets)
	b.mu.Unlock()
	var out []graywolf.Packet
	for _, p := range all {
		if (!q.Since.IsZero() && p.Timestamp.Before(q.Since)) || (q.Type != "" && q.Type != p.Type) ||
			(q.Direction != "" && !strings.EqualFold(q.Direction, p.Direction)) {
			continue
		}
		out = append(out, p)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}

// StationConfig is the radio's callsign.
func (b *Backend) StationConfig(ctx context.Context) (graywolf.StationConfig, error) {
	if call := b.ownCall(); call != "" {
		return graywolf.StationConfig{Callsign: call}, nil
	}
	_, err := b.session()
	return graywolf.StationConfig{}, apiErr(http.StatusServiceUnavailable, "/api/station/config", err.Error())
}

// SetStationCallsign sets the radio's callsign-SSID (firmware request 8).
func (b *Backend) SetStationCallsign(ctx context.Context, call string) (graywolf.StationConfig, error) {
	sess, err := b.session()
	if err != nil {
		return graywolf.StationConfig{}, apiErr(http.StatusServiceUnavailable, "/api/station/config", err.Error())
	}
	if err := sess.SetCallsign(ctx, call); err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, uvk5.ErrUnsupported) {
			code = http.StatusNotImplemented
		}
		return graywolf.StationConfig{}, apiErr(code, "/api/station/config", err.Error())
	}
	return b.StationConfig(ctx)
}

// State is the radio connection, for the admin pages and Check radio.
type State struct {
	Connected bool
	Since     time.Time
	Error     string
	Firmware  string
	Callsign  string
	APRSOn    bool
	Caps      uvk5.Capabilities
	Free      int
	LastRSSI  map[string]int // dBm by sender (firmware request 10)
	// Lent says who has the radio's port instead (UV Studio, the
	// firmware flasher); "": nobody.
	Lent string
}

// State reports the radio connection.
func (b *Backend) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := State{Callsign: b.callsign, Free: b.free, LastRSSI: map[string]int{}, Lent: b.lent}
	for k, v := range b.rssi {
		st.LastRSSI[k] = v
	}
	if b.sess == nil {
		if b.connErr != nil {
			st.Error = b.connErr.Error()
		}
		return st
	}
	set := b.sess.Settings()
	st.Connected, st.Since, st.Firmware, st.Caps, st.APRSOn = true, b.connected, b.sess.FirmwareVersion(), b.sess.Capabilities(), set.APRSOn
	return st
}

// RadioStatus reads battery, frequency and counters (firmware request 7).
func (b *Backend) RadioStatus(ctx context.Context) (uvk5.Status, error) {
	sess, err := b.session()
	if err != nil {
		return uvk5.Status{}, err
	}
	return sess.Status(ctx)
}
