package gwfake

import (
	"context"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
)

// dedupWindow is graywolf's inbound (from, msgid, text) dedup window: a
// repeat within it is ACKed again but not stored again.
const dedupWindow = 5 * time.Minute

// Frame is one APRS message (or ACK) on the simulated channel.
type Frame struct {
	From, To string
	Text     string
	MsgID    string
	IsAck    bool
}

// Profile describes how hostile the channel is. Every frame is
// independently dropped, duplicated, and delayed (which also reorders).
type Profile struct {
	Loss, Dup float64
	MaxDelay  time.Duration
}

type inFlight struct {
	at    time.Time
	frame Frame
}

// Radio is a shared simulated channel: every attached station hears
// every frame (subject to loss), as stations on one frequency do. It is
// driven by Deliver, so tests control time.
type Radio struct {
	Now  func() time.Time
	Prof Profile

	mu        sync.Mutex
	rng       *rand.Rand
	down      bool
	queue     []inFlight
	stations  []*Station
	sent      int
	delivered int
}

// NewRadio returns a channel with a seeded random source.
func NewRadio(seed uint64, prof Profile, now func() time.Time) *Radio {
	return &Radio{Now: now, Prof: prof, rng: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
}

// Attach puts s on the channel.
func (r *Radio) Attach(s *Station) {
	r.mu.Lock()
	r.stations = append(r.stations, s)
	r.mu.Unlock()
	s.mu.Lock()
	s.radio = r
	if s.heard == nil {
		s.heard = map[string]time.Time{}
	}
	s.mu.Unlock()
}

// SetDown drops every frame while down (a total outage).
func (r *Radio) SetDown(down bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.down = down
}

// Sent counts every frame put on air (data and ACKs), for airtime.
func (r *Radio) Sent() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sent
}

// Delivered counts frame copies handed to a station (after loss and
// duplication), for checking the channel model.
func (r *Radio) Delivered() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.delivered
}

// Inject puts a frame on air as if sent by anyone (e.g. a spoofer).
func (r *Radio) Inject(f Frame) { r.transmit(f) }

func (r *Radio) transmit(f Frame) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent++
	if r.down || r.rng.Float64() < r.Prof.Loss {
		return
	}
	copies := 1
	if r.rng.Float64() < r.Prof.Dup {
		copies = 2
	}
	for range copies {
		var delay time.Duration
		if r.Prof.MaxDelay > 0 {
			delay = time.Duration(r.rng.Int64N(int64(r.Prof.MaxDelay) + 1))
		}
		r.queue = append(r.queue, inFlight{at: r.Now().Add(delay), frame: f})
	}
}

// Deliver hands every frame due by now to the stations it's for, in
// arrival order. Frames a station sends in response are queued for a
// later Deliver.
func (r *Radio) Deliver() {
	now := r.Now()
	r.mu.Lock()
	var due []inFlight
	rest := r.queue[:0]
	for _, f := range r.queue {
		if f.at.After(now) {
			rest = append(rest, f)
		} else {
			due = append(due, f)
		}
	}
	r.queue = rest
	r.delivered += len(due)
	stations := slices.Clone(r.stations)
	r.mu.Unlock()
	slices.SortStableFunc(due, func(a, b inFlight) int { return a.at.Compare(b.at) })
	for _, f := range due {
		for _, s := range stations {
			if strings.EqualFold(s.Call, f.frame.To) && !strings.EqualFold(s.Call, f.frame.From) {
				s.hear(f.frame)
			}
		}
	}
}

// air puts a frame this station sends on the channel. Caller holds s.mu.
func (s *Station) air(f Frame) {
	if s.radio != nil {
		s.radio.transmit(f)
	}
}

// hear handles a frame addressed to this station as graywolf does: an
// ACK marks the matching sent row acked; a DM is stored unless it's a
// repeat within the dedup window, and is ACKed either way.
func (s *Station) hear(f Frame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logRX(f)
	if f.IsAck {
		for _, m := range s.rows {
			if m.Direction == "out" && m.MsgID == f.MsgID && strings.EqualFold(m.PeerCall, f.From) && m.Status != graywolf.StatusAcked {
				m.Status = graywolf.StatusAcked
				now := s.Now().UTC()
				m.AckedAt = &now
				s.touch(m.ID, graywolf.EventAcked)
			}
		}
		return
	}
	now := s.Now()
	key := f.From + "\x00" + f.MsgID + "\x00" + f.Text
	if last, ok := s.heard[key]; !ok || now.Sub(last) > dedupWindow {
		s.insert(graywolf.Message{
			Direction: "in", ThreadKind: graywolf.ThreadKindDM, FromCall: f.From, ToCall: s.Call, PeerCall: f.From,
			Text: f.Text, MsgID: f.MsgID, Status: graywolf.StatusReceived, Unread: true,
		})
	}
	s.heard[key] = now
	if f.MsgID != "" {
		s.air(Frame{From: s.Call, To: f.From, MsgID: f.MsgID, IsAck: true})
	}
}

// maxPacketLog bounds the fake's packet log, as graywolf's is bounded.
const maxPacketLog = 5000

// SetRXLevel sets the receive audio level reported for frames heard
// from call (as graywolf's modem measures it).
func (s *Station) SetRXLevel(call string, dbfs float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rxLevel == nil {
		s.rxLevel = map[string]float64{}
	}
	s.rxLevel[strings.ToUpper(call)] = dbfs
}

// logRX records a heard frame in the packet log. Caller holds s.mu.
func (s *Station) logRX(f Frame) {
	p := graywolf.Packet{
		Timestamp: s.Now().UTC(), Direction: "RX", Type: "message",
		Decoded: &graywolf.DecodedPacket{Source: f.From, Message: &graywolf.PacketMessage{
			Addressee: f.To, Text: f.Text, MessageID: f.MsgID, IsAck: f.IsAck,
		}},
	}
	if lvl, ok := s.rxLevel[strings.ToUpper(f.From)]; ok {
		p.AudioLevel = &graywolf.AudioLevel{LevelDBFS: lvl}
	}
	s.packets = append(s.packets, p)
	if len(s.packets) > maxPacketLog {
		s.packets = slices.Clone(s.packets[len(s.packets)-maxPacketLog:])
	}
}

// ListPackets implements the packet-log read (GET /api/packets), oldest
// first; Limit keeps the newest entries.
func (s *Station) ListPackets(ctx context.Context, q graywolf.PacketQuery) ([]graywolf.Packet, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []graywolf.Packet
	for _, p := range s.packets {
		switch {
		case !q.Since.IsZero() && p.Timestamp.Before(q.Since),
			q.Type != "" && p.Type != q.Type,
			q.Direction != "" && p.Direction != q.Direction:
			continue
		}
		out = append(out, p)
	}
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[len(out)-q.Limit:]
	}
	return out, nil
}
