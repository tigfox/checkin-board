package radiobackend

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

const (
	ownCall  = "KD2DCM-4"
	peerCall = "KD2DCM-1"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type harness struct {
	t     *testing.T
	b     *Backend
	st    *store.Store
	radio *uvk5.FakeRadio
	clk   *clock
	mu    sync.Mutex
	air   []uvk5.Message
	ports []net.Conn // the radio ends, one per connection
	dials int
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &harness{t: t, st: st, clk: &clock{t: time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)}}
	h.radio = uvk5.NewFakeRadio(ownCall, h.clk.Now)
	h.radio.Air = func(from, to string, m uvk5.Message) {
		h.mu.Lock()
		h.air = append(h.air, m)
		h.mu.Unlock()
	}
	h.b, err = New(Config{Store: st, Now: h.clk.Now, Dial: h.dial})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); h.b.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	h.waitFor("connected", func() bool { return h.b.State().Connected })
	return h
}

func (h *harness) dial(ctx context.Context) (Session, error) {
	host, dev := net.Pipe()
	go func() { _ = h.radio.Serve(dev) }()
	h.mu.Lock()
	h.ports = append(h.ports, dev)
	h.dials++
	h.mu.Unlock()
	h.t.Cleanup(func() { _ = dev.Close() })
	s, err := uvk5.Connect(ctx, host)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *harness) status(id uint64) string {
	m, err := h.b.GetMessage(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return m.Status
}

func (h *harness) send(text string) graywolf.Message {
	h.t.Helper()
	m, err := h.b.SendMessage(context.Background(), graywolf.SendRequest{To: strings.ToLower(peerCall), Text: text, ClientID: "c1"})
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// tick lets the radio's gap pass and transmit one item.
func (h *harness) tick() {
	h.clk.Add(5 * time.Second)
	h.radio.Tick()
}

func (h *harness) aired() []uvk5.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]uvk5.Message(nil), h.air...)
}

func TestSendSentAcked(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	var events []string
	var emu sync.Mutex
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		_ = h.b.StreamEventsWithOpen(sctx, nil, func(e graywolf.Event) error {
			emu.Lock()
			events = append(events, e.Type)
			emu.Unlock()
			return nil
		})
	}()
	time.Sleep(20 * time.Millisecond)

	m := h.send("RC1 H CTL 0 130000")
	if m.Status != graywolf.StatusQueued || m.PeerCall != peerCall || m.ClientID != "c1" || m.OurCall != ownCall {
		t.Fatalf("sent row = %+v", m)
	}
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	h.tick()
	h.waitFor("sent", func() bool { return h.status(m.ID) == graywolf.StatusSentRF })
	air := h.aired()
	if len(air) != 1 || air[0].ID != "01" || air[0].Text != "RC1 H CTL 0 130000" {
		t.Fatalf("air = %+v", air)
	}
	h.radio.Hear(peerCall, nil, uvk5.Message{Addressee: ownCall, ID: "01", Ack: true})
	h.waitFor("acked", func() bool { return h.status(m.ID) == graywolf.StatusAcked })
	got, _ := h.b.GetMessage(ctx, m.ID)
	if got.AckedAt == nil || got.SentAt == nil || got.MsgID != "01" || got.Attempts != 1 {
		t.Fatalf("acked row = %+v", got)
	}
	h.waitFor("events", func() bool {
		emu.Lock()
		defer emu.Unlock()
		return strings.Contains(strings.Join(events, ","), graywolf.EventAcked)
	})
	// A second ack (a repeat) changes nothing.
	h.radio.Hear(peerCall, nil, uvk5.Message{Addressee: ownCall, ID: "01", Ack: true})
	time.Sleep(20 * time.Millisecond)
	if h.status(m.ID) != graywolf.StatusAcked {
		t.Fatal("repeat ack changed the row")
	}
}

// The radio holds 4; the rest wait in the backend, never replacing.
func TestQueueNeverOverflows(t *testing.T) {
	h := newHarness(t)
	var ids []uint64
	for i := range 6 {
		ids = append(ids, h.send("m"+string(rune('0'+i))).ID)
	}
	h.waitFor("4 submitted", func() bool { return h.radio.Queued() == 4 })
	if h.status(ids[4]) != graywolf.StatusQueued || h.status(ids[5]) != graywolf.StatusQueued {
		t.Fatal("more than the radio's 4 were submitted")
	}
	for range 6 {
		h.tick()
		time.Sleep(20 * time.Millisecond)
	}
	h.waitFor("all sent", func() bool {
		for _, id := range ids {
			if h.status(id) != graywolf.StatusSentRF {
				return false
			}
		}
		return true
	})
	air := h.aired()
	if len(air) != 6 {
		t.Fatalf("aired %d, want 6", len(air))
	}
	for i, m := range air {
		if m.Text != "m"+string(rune('0'+i)) {
			t.Fatalf("out of order: %+v", air)
		}
	}
}

func TestReceiveDedupAndRead(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	gap := uvk5.Message{Addressee: ownCall, Text: "RC1 G CTL 3", ID: "050"}
	h.radio.Hear(peerCall, []string{"WIDE1-1*"}, gap)
	h.radio.Hear(peerCall, []string{"WIDE1-1*"}, gap)                                               // a repeat: dropped
	h.radio.Hear("KD2DCM-7", nil, uvk5.Message{Addressee: "KD2DCM-3", Text: "not for us", ID: "1"}) // logged, not stored
	var rows []graywolf.Message
	h.waitFor("a received row", func() bool {
		rows = nil
		_, _ = h.b.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderInbox}, func(c graywolf.MessageChange) error {
			rows = append(rows, *c.Message)
			return nil
		})
		return len(rows) >= 1
	})
	time.Sleep(50 * time.Millisecond)
	rows = nil
	cursor, _ := h.b.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderInbox}, func(c graywolf.MessageChange) error {
		rows = append(rows, *c.Message)
		return nil
	})
	if len(rows) != 1 || rows[0].Text != "RC1 G CTL 3" || rows[0].MsgID != "050" || !rows[0].Unread || rows[0].Via != "WIDE1-1*" || rows[0].Direction != "in" {
		t.Fatalf("inbox = %+v", rows)
	}
	if err := h.b.MarkRead(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	var after []graywolf.Message
	_, _ = h.b.CatchUp(ctx, graywolf.ListParams{Cursor: cursor}, func(c graywolf.MessageChange) error {
		after = append(after, *c.Message)
		return nil
	})
	if len(after) != 1 || after[0].Unread {
		t.Fatalf("after mark read: %+v", after)
	}
	pk, _ := h.b.ListPackets(ctx, graywolf.PacketQuery{Direction: "RX", Type: "message"})
	if len(pk) != 3 || pk[2].Decoded.Source != "KD2DCM-7" {
		t.Fatalf("packets = %+v", pk)
	}
	if pk, _ := h.b.ListPackets(ctx, graywolf.PacketQuery{Limit: 1}); len(pk) != 1 {
		t.Fatalf("limit: %d", len(pk))
	}
	// The radio acks it automatically.
	h.tick()
	if air := h.aired(); len(air) != 1 || !air[0].Ack || air[0].ID != "050" {
		t.Fatalf("auto-ack = %+v", air)
	}
}

func TestDropThenResend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.radio.SetBusy(true)
	m := h.send("stuck")
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	h.radio.Tick() // at the front: its minute starts
	h.clk.Add(61 * time.Second)
	h.radio.Tick()
	h.waitFor("failed", func() bool { return h.status(m.ID) == graywolf.StatusFailed })
	got, _ := h.b.GetMessage(ctx, m.ID)
	if !strings.Contains(got.FailureReason, "busy") {
		t.Fatalf("failure = %q", got.FailureReason)
	}
	h.radio.SetBusy(false)
	if _, err := h.b.ResendMessage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	h.waitFor("resubmitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	got, _ = h.b.GetMessage(ctx, m.ID)
	if got.MsgID != "02" {
		t.Fatalf("resend got line %q, want a new one", got.MsgID)
	}
}

func TestResendRules(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := h.send("pending")
	if _, err := h.b.ResendMessage(ctx, m.ID); !isStatus(err, 409) {
		t.Fatalf("pending: %v", err)
	}
	if _, err := h.b.ResendMessage(ctx, 999); !graywolf.IsNotFound(err) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := h.b.GetMessage(ctx, 999); !graywolf.IsNotFound(err) {
		t.Fatalf("get missing: %v", err)
	}
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	h.radio.Hear(peerCall, nil, uvk5.Message{Addressee: ownCall, ID: "01", Ack: true}) // the ack can beat SENT
	h.waitFor("acked", func() bool { return h.status(m.ID) == graywolf.StatusAcked })
	if _, err := h.b.ResendMessage(ctx, m.ID); !isStatus(err, 409) {
		t.Fatalf("acked: %v", err)
	}
	if err := h.b.DeleteMessage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.b.DeleteMessage(ctx, m.ID); !graywolf.IsNotFound(err) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := h.b.MarkRead(ctx, m.ID); !graywolf.IsNotFound(err) {
		t.Fatalf("mark read missing: %v", err)
	}
}

func TestRejectAndValidation(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	m := h.send("refuse me")
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	h.radio.Hear(peerCall, nil, uvk5.Message{Addressee: ownCall, ID: "01", Rej: true})
	h.waitFor("rejected", func() bool { return h.status(m.ID) == graywolf.StatusRejected })
	for _, req := range []graywolf.SendRequest{{To: "not a call", Text: "x"}, {To: peerCall, Text: ""}, {To: peerCall, Text: "a{b"}} {
		if _, err := h.b.SendMessage(ctx, req); !isStatus(err, 400) {
			t.Errorf("%+v: %v", req, err)
		}
	}
}

func TestReconnect(t *testing.T) {
	h := newHarness(t)
	h.mu.Lock()
	first := h.ports[0]
	h.mu.Unlock()
	_ = first.Close() // the radio is unplugged, then found again
	h.waitFor("a second connection", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.dials >= 2
	})
	h.waitFor("reconnected", func() bool { return h.b.State().Connected })
	m := h.send("after the reconnect")
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
}

// While the radio can't be reached, rows wait and the state says why.
func TestDialFailureKeepsRows(t *testing.T) {
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	b, _ := New(Config{Store: st, Dial: func(ctx context.Context) (Session, error) {
		return nil, uvk5.ErrFirmware
	}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); b.Run(ctx) }()
	defer func() { cancel(); <-done }()
	m, err := b.SendMessage(ctx, graywolf.SendRequest{To: peerCall, Text: "waits"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got, _ := b.GetMessage(ctx, m.ID); got.Status != graywolf.StatusQueued {
		t.Fatalf("status = %q", got.Status)
	}
	if st := b.State(); st.Connected || !strings.Contains(st.Error, "firmware") {
		t.Fatalf("state = %+v", st)
	}
	if _, err := b.StationConfig(ctx); err == nil {
		t.Fatal("station config without a radio")
	}
	if _, err := b.RadioStatus(ctx); err == nil {
		t.Fatal("status without a radio")
	}
}

// A submitted row the radio never reports on (its DROP lost to a
// reconnect) fails after a while, so the engines retry.
func TestSubmittedExpires(t *testing.T) {
	h := newHarness(t)
	h.radio.SetBusy(true)
	m := h.send("silence")
	h.waitFor("submitted", func() bool { return h.status(m.ID) == graywolf.StatusTxSubmitted })
	h.clk.Add(submittedTimeout)
	h.b.kick()
	h.waitFor("expired", func() bool { return h.status(m.ID) == graywolf.StatusFailed })
}

func TestStationAndPrefs(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if sc, err := h.b.StationConfig(ctx); err != nil || sc.Callsign != ownCall {
		t.Fatalf("station = %+v %v", sc, err)
	}
	if sc, err := h.b.SetStationCallsign(ctx, "KD2DCM-9"); err != nil || sc.Callsign != "KD2DCM-9" {
		t.Fatalf("set callsign = %+v %v", sc, err)
	}
	if p, _ := h.b.MessagePreferences(ctx); p.MaxText() != 67 {
		t.Fatalf("max text = %d", p.MaxText())
	}
	if p, _ := h.b.ConversationPrefs(ctx, graywolf.ThreadKindDM, peerCall); p.WaitForAck {
		t.Fatal("wait for ack on a radio")
	}
	if p, _ := h.b.SetConversationPrefs(ctx, graywolf.ThreadKindDM, peerCall, graywolf.ConversationPrefs{WaitForAck: true}); p.WaitForAck {
		t.Fatal("wait for ack accepted")
	}
	if st, err := h.b.RadioStatus(ctx); err != nil || st.FreqHz == 0 {
		t.Fatalf("status = %+v %v", st, err)
	}
	if st := h.b.State(); !st.Connected || st.Firmware == "" || !st.Caps.Has(uvk5.Required) || st.Free != 4 {
		t.Fatalf("state = %+v", st)
	}
}

func isStatus(err error, code int) bool {
	ae, ok := err.(*graywolf.APIError)
	return ok && ae.StatusCode == code
}
