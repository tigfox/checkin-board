package checkpoint

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

type fakeTime struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeTime) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

type harness struct {
	t   *testing.T
	s   *store.Store
	gw  *gwfake.Station
	e   *Engine
	ft  *fakeTime
	cfg store.Settings
}

func checkpointSettings() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceState = store.RoleCheckpoint, "AS5", "N0CALL-10", store.RaceActive
	return c
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	s, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ft := &fakeTime{t: t0}
	s.SetClock(ft.Now)
	gw := gwfake.New("N0CALL-1")
	gw.Now = ft.Now
	clock := raceclock.NewClock(ft.Now, func() bool { return true })
	e, err := New(Config{Store: s, Graywolf: gw, Clock: clock, Now: ft.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, s: s, gw: gw, e: e, ft: ft, cfg: checkpointSettings()}
}

func (h *harness) tick() {
	h.t.Helper()
	if err := h.e.Tick(ctx, h.cfg); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

func (h *harness) log(bib store.Bib) {
	h.t.Helper()
	if _, err := h.s.LogLocal(ctx, h.cfg.CheckpointCode, bib, h.ft.Now(), true); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) reports() []gwfake.Transmission { return h.gw.TransmissionsWithPrefix("RC1 R ") }

// distinctRows is the set of graywolf rows (batches) among txs.
func distinctRows(txs []gwfake.Transmission) map[uint64]bool {
	out := map[uint64]bool{}
	for _, t := range txs {
		out[t.ID] = true
	}
	return out
}

// ack has graywolf mark row id acked and tells the engine, as the inbox
// reader would.
func (h *harness) ack(id uint64) {
	h.t.Helper()
	h.gw.Ack(id)
	m, err := h.gw.GetMessage(ctx, id)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.e.HandleOutbound(ctx, m); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) outbox() store.OutboxStats {
	st, err := h.s.OutboxStats(ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestFlushesAfterAge(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.tick()
	if n := len(h.reports()); n != 0 {
		t.Fatalf("batched a fresh lone entry: %d reports", n)
	}
	h.ft.Advance(19 * time.Second)
	h.tick()
	if n := len(h.reports()); n != 0 {
		t.Fatalf("flushed before flush_after_sec: %d", n)
	}
	h.ft.Advance(time.Second)
	h.tick()
	got := h.reports()
	if len(got) != 1 || got[0].To != "N0CALL-10" || got[0].Text != "RC1 R AS5 1 @1300 101/00" || got[0].Resend {
		t.Fatalf("reports = %+v", got)
	}
	pending, _ := h.s.ListPendingBatches(ctx)
	if len(pending) != 1 || pending[0].GWMessageID == nil || *pending[0].GWMessageID != got[0].ID {
		t.Fatalf("batch not bound to its graywolf row: %+v", pending)
	}
}

func TestFlushesImmediatelyWhenAFrameIsFull(t *testing.T) {
	h := newHarness(t)
	for i := range fullBatchEntries(h.cfg.MaxTextLen) {
		h.log(store.Bib(1000 + i))
	}
	h.tick()
	if n := len(h.reports()); n != 1 {
		t.Fatalf("reports = %d, want an immediate full batch", n)
	}
}

func TestRetryBackoffResendsSameRow(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	first := h.reports()[0].ID
	// Resends at +30, +60, +120, then every 300 s, always of the same row.
	for _, wait := range []time.Duration{30, 60, 120, 300, 300} {
		h.ft.Advance(wait*time.Second - time.Second)
		h.tick()
		before := len(h.reports())
		h.ft.Advance(time.Second)
		h.tick()
		got := h.reports()
		if len(got) != before+1 {
			t.Fatalf("after %ds: %d reports, want %d", wait, len(got), before+1)
		}
		if last := got[len(got)-1]; !last.Resend || last.ID != first {
			t.Fatalf("retry was %+v, want a resend of row %d", last, first)
		}
	}
}

func TestInFlightWindow(t *testing.T) {
	h := newHarness(t)
	for i := range 6 {
		h.log(store.Bib(i + 1))
		h.ft.Advance(20 * time.Second)
		h.tick()
	}
	if n := len(distinctRows(h.reports())); n != h.cfg.MaxInFlight {
		t.Fatalf("sent %d batches with none ACKed, want window %d", n, h.cfg.MaxInFlight)
	}
	h.ack(h.reports()[0].ID)
	h.tick()
	if n := len(distinctRows(h.reports())); n != h.cfg.MaxInFlight+1 {
		t.Fatalf("after one ACK sent %d batches, want %d", n, h.cfg.MaxInFlight+1)
	}
}

func TestAckConfirmsEntriesAndRecordsContact(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.log(102)
	h.ft.Advance(20 * time.Second)
	h.tick()
	if !h.e.LastContact().IsZero() {
		t.Fatal("contact recorded before any ACK")
	}
	h.ft.Advance(5 * time.Second)
	h.ack(h.reports()[0].ID)
	if st := h.outbox(); st.Unconfirmed != 0 || st.PendingBatches != 0 {
		t.Fatalf("outbox = %+v", st)
	}
	if !h.e.LastContact().Equal(h.ft.Now()) {
		t.Fatalf("LastContact = %v, want %v", h.e.LastContact(), h.ft.Now())
	}
	// Re-delivery of the same acked row (the feed shows it again) is a no-op.
	h.ack(h.reports()[0].ID)
}

func TestOutboundForOtherPeersIgnored(t *testing.T) {
	h := newHarness(t)
	h.tick()
	m := graywolf.Message{ID: 999, Direction: "out", ToCall: "SOMEONE", Text: "RC1 H AS5 0 130000", Status: graywolf.StatusAcked}
	if err := h.e.HandleOutbound(ctx, m); err != nil {
		t.Fatal(err)
	}
	if !h.e.LastContact().IsZero() {
		t.Fatal("ACK from a non-HQ peer counted as HQ contact")
	}
}

func TestPollCatchesAcksTheFeedMissed(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	h.gw.Ack(h.reports()[0].ID) // graywolf knows; the feed never told us
	h.ft.Advance(pollEvery)
	h.tick()
	if st := h.outbox(); st.PendingBatches != 0 || st.Unconfirmed != 0 {
		t.Fatalf("outbox = %+v, want the poll to confirm the batch", st)
	}
}

func TestRejectedStatusParksBatch(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	id := h.reports()[0].ID
	h.gw.Reject(id)
	m, _ := h.gw.GetMessage(ctx, id)
	if err := h.e.HandleOutbound(ctx, m); err != nil {
		t.Fatal(err)
	}
	if st := h.outbox(); st.RejectedBatches != 1 || st.PendingBatches != 0 {
		t.Fatalf("outbox = %+v", st)
	}
}

func TestGapRequestFromHQResends(t *testing.T) {
	h := newHarness(t)
	for i := range 2 {
		h.log(store.Bib(i + 1))
		h.ft.Advance(20 * time.Second)
		h.tick()
	}
	first := h.reports()[0].ID
	h.ack(first)
	h.ack(h.reports()[1].ID)
	h.ft.Advance(2 * time.Minute)
	before := len(h.reports())

	// HQ lost batch 1 and asks for it.
	gap := h.gw.Inbound("N0CALL-10", "RC1 G AS5 1")
	if err := h.e.HandleInbound(ctx, gap); err != nil {
		t.Fatal(err)
	}
	h.tick()
	got := h.reports()
	if len(got) != before+1 {
		t.Fatalf("reports = %d, want one resend", len(got)-before)
	}
	// It was acked, so its old row would read acked at once: it goes as new.
	if last := got[len(got)-1]; last.Resend || last.ID == first || !strings.HasPrefix(last.Text, "RC1 R AS5 1 ") {
		t.Fatalf("gap resend = %+v, want a new message", last)
	}
	if !h.e.LastContact().Equal(h.ft.Now()) {
		t.Error("gap request not counted as HQ contact")
	}
}

func TestInboundFromOthersIgnored(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	h.ack(h.reports()[0].ID)
	h.ft.Advance(2 * time.Minute)
	before := len(h.reports())
	for _, m := range []graywolf.Message{
		h.gw.Inbound("SPOOF", "RC1 G AS5 1"),
		h.gw.Inbound("N0CALL-10", "RC1 G AS5 garbage"),
		h.gw.Inbound("N0CALL-10", "RC1 P FIN 3 1/5"),      // link check: phase 12
		h.gw.Inbound("N0CALL-10", "RC1 H OTHER 0 130000"), // not for a checkpoint
	} {
		if err := h.e.HandleInbound(ctx, m); err != nil {
			t.Fatalf("%q: %v", m.Text, err)
		}
	}
	h.tick()
	if n := len(h.reports()) - before; n != 0 {
		t.Fatalf("sent %d reports for ignored traffic", n)
	}
}

func TestSendFailureRetriesNextTick(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.gw.FailNextSend(&graywolf.APIError{StatusCode: http.StatusServiceUnavailable, Message: "down"})
	if err := h.e.Tick(ctx, h.cfg); err == nil {
		t.Fatal("expected send error")
	}
	pending, _ := h.s.ListPendingBatches(ctx)
	if len(pending) != 1 || pending[0].Attempts != 0 || pending[0].GWMessageID != nil {
		t.Fatalf("after failed send = %+v, want schedule restored", pending)
	}
	h.tick()
	if n := len(h.reports()); n != 1 {
		t.Fatalf("reports = %d, want the retry next tick", n)
	}
}

func TestGraywolfRefusalAlertsAndKeepsRetrying(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.gw.FailNextSend(&graywolf.APIError{StatusCode: http.StatusBadRequest, Message: "bad path"})
	if err := h.e.Tick(ctx, h.cfg); err == nil {
		t.Fatal("expected error surfaced")
	}
	if r := h.e.LastRefusal(); r.Reason != "bad path" || r.At.IsZero() {
		t.Fatalf("refusal = %+v", r)
	}
	if st := h.outbox(); st.PendingBatches != 1 || st.RejectedBatches != 0 {
		t.Fatalf("outbox = %+v, want the batch kept pending, not parked", st)
	}
	// It keeps its retry-ladder slot (no hammering graywolf every tick)...
	h.ft.Advance(time.Second)
	h.tick()
	if n := len(h.reports()); n != 0 {
		t.Fatalf("retried a refused batch the next second: %d", n)
	}
	// ...and goes out once the operator fixes the settings.
	h.ft.Advance(30 * time.Second)
	h.tick()
	if n := len(h.reports()); n != 1 {
		t.Fatalf("reports = %d, want the retry", n)
	}
	if r := h.e.LastRefusal(); !r.At.IsZero() {
		t.Errorf("refusal not cleared by a successful send: %+v", r)
	}
}

func TestUnknownSendOutcomeIsRecoveredNotDuplicated(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	// The POST reached graywolf (row created) but the response was lost.
	h.gw.FailNextSend(errors.New("context deadline exceeded"))
	_ = h.e.Tick(ctx, h.cfg)
	pending, _ := h.s.ListPendingBatches(ctx)
	if pending[0].Attempts != 1 {
		t.Fatalf("attempt rolled back after an unknown outcome: %+v", pending[0])
	}
	sent, _ := h.gw.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: pending[0].Text})
	h.ft.Advance(time.Second)
	h.tick() // recoverUnbound finds and binds the row
	pending, _ = h.s.ListPendingBatches(ctx)
	if pending[0].GWMessageID == nil || *pending[0].GWMessageID != sent.ID {
		t.Fatalf("batch = %+v, want bound to row %d", pending[0], sent.ID)
	}
	h.ft.Advance(30 * time.Second)
	h.tick()
	// The simulated lost-response send plus one resend, both on that row:
	// no second message was created.
	got := h.reports()
	if len(got) != 2 || got[0].ID != sent.ID || got[1].ID != sent.ID || !got[1].Resend {
		t.Fatalf("reports = %+v, want only row %d (original + resend)", got, sent.ID)
	}
}

func TestReplayedAckWithoutTimeIsNotContact(t *testing.T) {
	h := newHarness(t)
	h.gw.NoAckedAt = true
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	id := h.reports()[0].ID
	h.ack(id) // confirms: counts as contact
	first := h.e.LastContact()
	if first.IsZero() {
		t.Fatal("confirming ACK not counted")
	}
	h.ft.Advance(time.Hour)
	h.ack(id) // the same row replayed an hour later
	if !h.e.LastContact().Equal(first) {
		t.Fatalf("replayed ACK moved contact to %v", h.e.LastContact())
	}
}

func TestResendConflictKeepsSchedule(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	id := h.reports()[0].ID
	h.gw.SetInFlight(id, true)
	h.ft.Advance(30 * time.Second)
	h.tick() // 409: graywolf is already sending it; not an error
	pending, _ := h.s.ListPendingBatches(ctx)
	if len(pending) != 1 || pending[0].Attempts != 2 {
		t.Fatalf("after 409 = %+v, want the attempt counted", pending)
	}
}

func TestResendOfDeletedRowSendsNew(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	old := h.reports()[0].ID
	h.gw.Delete(old) // operator deleted it in graywolf
	h.ft.Advance(30 * time.Second)
	h.tick()
	got := h.reports()
	if len(got) != 2 || got[1].Resend || got[1].ID == old {
		t.Fatalf("reports = %+v, want a new message", got)
	}
	h.ack(got[1].ID)
	if st := h.outbox(); st.PendingBatches != 0 {
		t.Fatalf("outbox = %+v", st)
	}
}

func TestHeartbeat(t *testing.T) {
	h := newHarness(t)
	h.tick()
	hbs := h.gw.TransmissionsWithPrefix("RC1 H ")
	if len(hbs) != 1 || hbs[0].Text != "RC1 H AS5 0 130000" || hbs[0].To != "N0CALL-10" {
		t.Fatalf("heartbeats = %+v", hbs)
	}
	if known, _ := h.s.KnownGWRow(ctx, hbs[0].ID); !known {
		t.Error("heartbeat row not recorded for cleanup")
	}
	h.ft.Advance(time.Duration(h.cfg.HeartbeatSec)*time.Second - time.Second)
	h.tick()
	if n := len(h.gw.TransmissionsWithPrefix("RC1 H ")); n != 1 {
		t.Fatalf("heartbeat early: %d", n)
	}
	h.ft.Advance(time.Second)
	h.tick()
	if n := len(h.gw.TransmissionsWithPrefix("RC1 H ")); n != 2 {
		t.Fatalf("heartbeats = %d, want 2", n)
	}
	// An ACK of a heartbeat proves HQ is reachable.
	h.ack(hbs[0].ID)
	if h.e.LastContact().IsZero() {
		t.Error("heartbeat ACK not counted as contact")
	}
}

func TestHeartbeatFailureRetriesSoon(t *testing.T) {
	h := newHarness(t)
	h.gw.FailNextSend(errors.New("graywolf down"))
	if err := h.e.Tick(ctx, h.cfg); err == nil {
		t.Fatal("expected heartbeat error")
	}
	h.ft.Advance(heartbeatRetry)
	h.tick()
	if n := len(h.gw.TransmissionsWithPrefix("RC1 H ")); n != 1 {
		t.Fatalf("heartbeats = %d, want a retry after %v", n, heartbeatRetry)
	}
}

func TestFastRetransmitOnConfirmingAck(t *testing.T) {
	h := newHarness(t)
	h.log(1)
	h.ft.Advance(20 * time.Second)
	h.tick() // batch 1 sent at t0+20; its normal retry is due at t0+50
	first := h.reports()[0].ID
	h.ft.Advance(time.Second)
	h.log(2)
	h.ft.Advance(20 * time.Second)
	h.tick() // batch 2 sent at t0+41
	h.ft.Advance(4 * time.Second)

	// At t0+45 HQ confirms batch 2: the link works, and batch 1 (25 s
	// old, retry not due until t0+50) must have been lost.
	before := len(h.reports())
	h.ack(h.reports()[1].ID)
	h.tick()
	got := h.reports()[before:]
	if len(got) != 1 || got[0].ID != first || !got[0].Resend {
		t.Fatalf("after confirming ACK sent %+v, want batch 1 resent at once", got)
	}

	// A duplicate ACK doesn't expedite again.
	h.ft.Advance(fastRetransmitAfter)
	before = len(h.reports())
	h.ack(h.reports()[1].ID)
	h.tick()
	if n := len(h.reports()) - before; n != 0 {
		t.Fatalf("duplicate ACK expedited %d", n)
	}
}

func TestNoFastRetransmitForRecentSends(t *testing.T) {
	h := newHarness(t)
	h.log(1)
	h.ft.Advance(20 * time.Second)
	h.tick() // batch 1 at t0+20
	h.log(2)
	h.ft.Advance(20 * time.Second)
	h.tick() // batch 2 at t0+40; batch 1 is due at t0+50
	before := len(h.reports())
	h.ft.Advance(5 * time.Second) // t0+45: batch 1 only 25 s old... batch 2 is 5 s old
	h.ack(h.reports()[0].ID)      // confirm batch 1; batch 2 is too recent to be "lost"
	h.tick()
	if n := len(h.reports()) - before; n != 0 {
		t.Fatalf("resent %d batches younger than %v", n, fastRetransmitAfter)
	}
}

func TestMaxTextUsesGraywolfLimit(t *testing.T) {
	h := newHarness(t)
	h.cfg.MaxTextLen = 200
	for i := range 30 {
		h.log(store.Bib(1000 + i))
	}
	h.ft.Advance(20 * time.Second)
	h.tick() // graywolf still at the default 67
	for _, r := range h.reports() {
		if len(r.Text) > wire.DefaultMaxTextLen {
			t.Fatalf("sent %d chars with graywolf at 67: %q", len(r.Text), r.Text)
		}
	}
	h.e.SetGraywolfMaxText(200)
	if got := h.e.maxText(h.cfg); got != 200 {
		t.Fatalf("maxText = %d, want 200 once graywolf allows it", got)
	}
}

func TestRecoverBindsBatchesSentBeforeACrash(t *testing.T) {
	h := newHarness(t)
	h.gw.EchoClientID = true
	h.log(101)
	h.log(102)
	h.ft.Advance(20 * time.Second)
	// Simulate: batch created and attempted, POST succeeded, crash before bind.
	b, _ := h.s.CreateBatch(ctx, "AS5", 67, h.ft.Now())
	_ = h.s.MarkTransmitted(ctx, b.ID, h.ft.Now(), h.ft.Now().Add(30*time.Second))
	sent, _ := h.gw.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: b.Text, ClientID: b.ClientID})

	if err := h.e.Recover(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	pending, _ := h.s.ListPendingBatches(ctx)
	if len(pending) != 1 || pending[0].GWMessageID == nil || *pending[0].GWMessageID != sent.ID {
		t.Fatalf("after recover = %+v, want bound to row %d", pending, sent.ID)
	}
}

func TestRecoverFallsBackToTextAndLeavesUnsentAlone(t *testing.T) {
	h := newHarness(t) // graywolf doesn't keep client_id
	h.log(101)
	h.ft.Advance(20 * time.Second)
	b1, _ := h.s.CreateBatch(ctx, "AS5", 67, h.ft.Now())
	_ = h.s.MarkTransmitted(ctx, b1.ID, h.ft.Now(), h.ft.Now().Add(30*time.Second))
	sent, _ := h.gw.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: b1.Text})
	h.log(102)
	b2, _ := h.s.CreateBatch(ctx, "AS5", 67, h.ft.Now())
	_ = h.s.MarkTransmitted(ctx, b2.ID, h.ft.Now(), h.ft.Now().Add(30*time.Second)) // POST never happened

	if err := h.e.Recover(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	unbound, _ := h.s.UnboundAttempted(ctx)
	if len(unbound) != 1 || unbound[0].ID != b2.ID {
		t.Fatalf("unbound = %+v, want only batch 2", unbound)
	}
	pending, _ := h.s.ListPendingBatches(ctx)
	if pending[0].GWMessageID == nil || *pending[0].GWMessageID != sent.ID {
		t.Fatalf("batch 1 = %+v, want bound by text to row %d", pending[0], sent.ID)
	}
}

func TestRecoverIgnoresRowsFromAnEarlierRace(t *testing.T) {
	h := newHarness(t)
	// A rehearsal sent the identical text an hour ago and it was acked.
	old, _ := h.gw.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "RC1 R AS5 1 @1300 101/00"})
	h.gw.Ack(old.ID)
	h.ft.Advance(time.Hour)
	h.ft.t = t0.Add(24 * time.Hour) // race day, same time of day
	h.log(101)
	h.ft.Advance(20 * time.Second)
	b, _ := h.s.CreateBatch(ctx, "AS5", 67, h.ft.Now())
	_ = h.s.MarkTransmitted(ctx, b.ID, h.ft.Now(), h.ft.Now().Add(30*time.Second))
	if err := h.e.Recover(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	if unbound, _ := h.s.UnboundAttempted(ctx); len(unbound) != 1 {
		t.Fatalf("batch bound to the rehearsal's row: unbound = %+v", unbound)
	}
}

func TestBackoffSchedule(t *testing.T) {
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		if got := retryBackoff(i + 1); got != w {
			t.Errorf("retryBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestFullBatchEntriesFitsWorstCase(t *testing.T) {
	for _, maxLen := range []int{67, 100, 200} {
		n := fullBatchEntries(maxLen)
		entries := make([]wire.Entry, n)
		for i := range entries {
			// One minute group: fullBatchEntries is only the early-flush
			// trigger; entries spanning minutes split across frames.
			entries[i] = wire.Entry{Bib: 9999, Time: wire.TimeOfDay(59), Void: true}
		}
		_, packed, err := wire.PackReport("CP1234", 4294967295, entries, maxLen)
		if err != nil || packed != n {
			t.Errorf("maxLen %d: %d worst-case entries packed %d, %v", maxLen, n, packed, err)
		}
	}
}

// panicky panics on the first SendMessage, then delegates.
type panicky struct {
	*gwfake.Station
	once sync.Once
}

func (p *panicky) SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error) {
	panicked := false
	p.once.Do(func() { panicked = true })
	if panicked {
		panic("boom")
	}
	return p.Station.SendMessage(ctx, req)
}

func TestRunSurvivesPanicAndStops(t *testing.T) {
	h := newHarness(t)
	gw := &panicky{Station: h.gw}
	e, err := New(Config{Store: h.s, Graywolf: gw, Clock: raceclock.NewClock(nil, nil), Interval: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.SaveSettings(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.gw.TransmissionsWithPrefix("RC1 H ")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat after the panicking tick")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestRunIdleUnlessCheckpointRole(t *testing.T) {
	h := newHarness(t)
	e, _ := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(nil, nil), Interval: 5 * time.Millisecond})
	hq := store.DefaultSettings()
	hq.Role, hq.HQLocalCodes = store.RoleHQ, "FIN"
	if _, err := h.s.SaveSettings(ctx, hq); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	_ = e.Run(runCtx)
	if n := len(h.gw.Transmissions()); n != 0 {
		t.Fatalf("HQ node's checkpoint engine transmitted %d frames", n)
	}
}

func TestTurnsOffGraywolfRetriesForHQBeforeSending(t *testing.T) {
	h := newHarness(t)
	e, err := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(h.ft.Now, nil), Now: h.ft.Now,
		Peers: peers.NewEnsurer(h.gw, h.s, h.ft.Now)})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	if p, _ := h.gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10"); p.WaitForAck {
		t.Fatal("graywolf retries still on for HQ after the first send")
	}
	if b, _ := h.s.ListPeerPrefs(ctx); len(b) != 1 || !b[0].WaitForAck {
		t.Fatalf("original prefs not backed up: %+v", b)
	}
	// A prefs failure doesn't block delivery.
	h2 := newHarness(t)
	h2.gw.FailPrefs(errors.New("prefs endpoint missing"))
	e2, _ := New(Config{Store: h2.s, Graywolf: h2.gw, Clock: raceclock.NewClock(h2.ft.Now, nil), Now: h2.ft.Now,
		Peers: peers.NewEnsurer(h2.gw, h2.s, h2.ft.Now)})
	if err := e2.Tick(ctx, h2.cfg); err != nil {
		t.Fatal(err)
	}
	if n := len(h2.gw.TransmissionsWithPrefix("RC1 H ")); n != 1 {
		t.Fatalf("heartbeats = %d; a prefs failure blocked sending", n)
	}
}

func TestNothingOnAirOutsideSendingStates(t *testing.T) {
	for _, state := range []string{store.RaceSetup, store.RaceSecured, store.RaceCheckedIn} {
		h := newHarness(t)
		h.cfg.RaceState = state
		h.log(101)
		h.ft.Advance(time.Hour)
		h.tick()
		if n := len(h.gw.Transmissions()); n != 0 {
			t.Errorf("%s: %d frames on air", state, n)
		}
	}
}

func TestCompleteFlushesWithoutWaiting(t *testing.T) {
	h := newHarness(t)
	h.cfg.RaceState = store.RaceComplete
	h.log(101) // last runner, keypad now closed
	h.tick()
	if n := len(h.reports()); n != 1 {
		t.Fatalf("reports = %d, want the lone entry sent at once", n)
	}
}

func TestFinalCheckInDeliversEverythingThenCompletes(t *testing.T) {
	h := newHarness(t)
	called := 0
	e, err := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(h.ft.Now, func() bool { return true }),
		Now: h.ft.Now, OnCheckedIn: func(context.Context) error { called++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	for bib := store.Bib(1); bib <= 4; bib++ { // a full frame: sent at once
		h.log(bib)
	}
	h.tick()
	for bib := store.Bib(5); bib <= 8; bib++ {
		h.log(bib)
	}
	h.tick() // two batches out, neither ACKed yet (out of range)
	if n := len(distinctRows(h.reports())); n != 2 {
		t.Fatalf("batches sent = %d", n)
	}
	h.ack(h.reports()[0].ID) // HQ got the first one

	// Packed up for the trip back to HQ: radio quiet.
	h.cfg.RaceState = store.RaceSecured
	if _, err := h.s.SaveSettings(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	before := len(h.gw.Transmissions())
	h.ft.Advance(2 * time.Hour)
	h.tick()
	if n := len(h.gw.Transmissions()) - before; n != 0 {
		t.Fatalf("secured node transmitted %d frames", n)
	}

	// At HQ: final check-in.
	h.cfg.RaceState = store.RaceCheckingIn
	_, _ = h.s.SaveSettings(ctx, h.cfg)
	before = len(h.gw.Transmissions())
	h.tick()
	sent := h.gw.Transmissions()[before:]
	var resent, heartbeats int
	for _, tx := range sent {
		switch {
		case strings.HasPrefix(tx.Text, "RC1 R "):
			resent++
		case strings.HasPrefix(tx.Text, "RC1 H "):
			heartbeats++
		}
	}
	if resent != 1 || heartbeats != 1 {
		t.Fatalf("check-in sent %d batches and %d heartbeats, want the unconfirmed batch and a heartbeat at once", resent, heartbeats)
	}
	h.tick()
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceCheckingIn {
		t.Fatalf("state = %s before HQ confirmed", got.RaceState)
	}
	h.ft.Advance(time.Second)
	h.ack(h.reports()[len(h.reports())-1].ID)
	h.tick()
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceCheckedIn || called != 1 {
		t.Fatalf("state = %s, OnCheckedIn calls = %d", got.RaceState, called)
	}
}

func TestCheckInNeedsFreshContactEvenWhenNothingIsPending(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	h.ack(h.reports()[0].ID) // all confirmed during the race
	h.cfg.RaceState = store.RaceCheckingIn
	_, _ = h.s.SaveSettings(ctx, h.cfg)
	h.ft.Advance(time.Hour)
	h.tick()
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceCheckingIn {
		t.Fatal("checked in without hearing HQ since the check-in began")
	}
	hb := h.gw.TransmissionsWithPrefix("RC1 H ")
	h.ack(hb[len(hb)-1].ID) // HQ answers the check-in heartbeat
	h.tick()
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceCheckedIn {
		t.Fatalf("state = %s, want checked_in", got.RaceState)
	}
}

func TestReset(t *testing.T) {
	h := newHarness(t)
	h.log(101)
	h.ft.Advance(20 * time.Second)
	h.tick()
	h.ack(h.reports()[0].ID)
	h.e.Reset()
	if !h.e.LastContact().IsZero() {
		t.Fatal("contact survived Reset")
	}
	before := len(h.gw.TransmissionsWithPrefix("RC1 H "))
	h.tick() // heartbeat schedule restarts
	if n := len(h.gw.TransmissionsWithPrefix("RC1 H ")); n != before+1 {
		t.Fatalf("heartbeats = %d, want one right after Reset", n-before)
	}
}

// checkInReady sets up a checking_in node whose HQ just ACKed the
// check-in heartbeat, so the next tick would finish the check-in.
func checkInReady(t *testing.T, hook func(context.Context) error, gwLag time.Duration) *harness {
	t.Helper()
	h := newHarness(t)
	e, err := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(h.ft.Now, func() bool { return true }),
		Now: h.ft.Now, OnCheckedIn: hook})
	if err != nil {
		t.Fatal(err)
	}
	h.e = e
	h.gw.Now = func() time.Time { return h.ft.Now().Add(-gwLag) } // graywolf's own clock
	h.cfg.RaceState = store.RaceCheckingIn
	if _, err := h.s.SaveSettings(ctx, h.cfg); err != nil {
		t.Fatal(err)
	}
	h.tick() // check-in starts: heartbeat out
	h.ft.Advance(2 * time.Second)
	hb := h.gw.TransmissionsWithPrefix("RC1 H ")
	h.ack(hb[len(hb)-1].ID)
	return h
}

func TestCheckInUsesThisNodesClockNotGraywolfs(t *testing.T) {
	// graywolf's clock is 10 minutes behind: its acked_at predates the
	// check-in, but HQ was heard after it began.
	h := checkInReady(t, nil, 10*time.Minute)
	h.tick()
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceCheckedIn {
		t.Fatalf("state = %s; a lagging graywolf clock blocked the check-in", got.RaceState)
	}
}

func TestFinishCheckInLosesToAConcurrentOperatorAction(t *testing.T) {
	called := 0
	h := checkInReady(t, func(context.Context) error { called++; return nil }, 0)
	stale := h.cfg // the tick loaded checking_in...
	// ...while the operator secures the node again.
	if ok, err := h.s.SetRaceState(ctx, []string{store.RaceCheckingIn}, store.RaceSecured, nil); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if err := h.e.Tick(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.s.GetSettings(ctx); got.RaceState != store.RaceSecured || called != 0 {
		t.Fatalf("state = %s, hook calls = %d; the engine overwrote the operator", got.RaceState, called)
	}
}

func TestCheckedInHookRetriedUntilItSucceeds(t *testing.T) {
	calls := 0
	h := checkInReady(t, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("graywolf prefs endpoint down")
		}
		return nil
	}, 0)
	if err := h.e.Tick(ctx, h.cfg); err == nil {
		t.Fatal("expected the hook's error")
	}
	cfg, _ := h.s.GetSettings(ctx)
	h.tick2(cfg)
	h.tick2(cfg)
	if calls != 2 {
		t.Fatalf("hook calls = %d, want one retry then done", calls)
	}
	// After a restart in checked_in, the hook runs again (idempotent).
	h.e.Reset()
	h.tick2(cfg)
	if calls != 3 {
		t.Fatalf("hook calls after restart = %d", calls)
	}
}

func (h *harness) tick2(cfg store.Settings) {
	h.t.Helper()
	if err := h.e.Tick(ctx, cfg); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}
