package hq

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

func hqSettings() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceState = store.RoleHQ, "START,FIN", store.RaceActive
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
	gw := gwfake.New("N0CALL-10")
	gw.Now = ft.Now
	e, err := New(Config{Store: s, Graywolf: gw, Clock: raceclock.NewClock(ft.Now, func() bool { return true }), Now: ft.Now})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, s: s, gw: gw, e: e, ft: ft, cfg: hqSettings()}
}

func (h *harness) tick() {
	h.t.Helper()
	if err := h.e.Tick(ctx, h.cfg); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

// receive delivers text from `from` the way the inbox reader would.
func (h *harness) receive(from, text string) {
	h.t.Helper()
	m := h.gw.Inbound(from, text)
	if err := h.e.HandleInbound(ctx, m); err != nil {
		h.t.Fatalf("HandleInbound(%q): %v", text, err)
	}
}

func (h *harness) gaps() []gwfake.Transmission { return h.gw.TransmissionsWithPrefix("RC1 G ") }

// run ticks every 10 s of simulated time.
func (h *harness) run(d time.Duration) {
	h.t.Helper()
	for end := h.ft.Now().Add(d); h.ft.Now().Before(end); {
		h.ft.Advance(10 * time.Second)
		h.tick()
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestReportIngestedAndRowRecorded(t *testing.T) {
	h := newHarness(t)
	m := h.gw.Inbound("N0CALL-7", "RC1 R AS5 1 @1300 101/05 102/30")
	if err := h.e.HandleInbound(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, _ := h.s.EffectiveEntries(ctx, store.EntryFilter{})
	if len(got) != 2 || got[0].SourceCall != "N0CALL-7" || !got[0].TimeIn.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("entries = %+v", got)
	}
	if known, _ := h.s.KnownGWRow(ctx, m.ID); !known {
		t.Error("graywolf row not recorded with the batch")
	}
	// A second copy (graywolf dedup window passed) changes nothing.
	h.receive("N0CALL-7", "RC1 R AS5 1 @1300 101/05 102/30")
	if got, _ := h.s.EffectiveEntries(ctx, store.EntryFilter{}); len(got) != 2 || got[0].Count != 1 {
		t.Fatalf("after duplicate: %+v", got)
	}
}

func TestHeartbeatRecordsStatusAndSkew(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 H AS5 0 125950") // 10 s behind HQ's clock
	sts, _ := h.s.ListStatuses(ctx)
	if len(sts) != 1 || sts[0].HeartbeatAt == nil || sts[0].ClockSkewSec == nil || *sts[0].ClockSkewSec != -10 {
		t.Fatalf("status = %+v", sts)
	}
}

func TestUndecodableReportIsKeptNotRetried(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 1 @1300 101/05") // AS5 known now
	m := h.gw.Inbound("N0CALL-1", "RC1 R AS5 2 @1360 7/00")
	if err := h.e.HandleInbound(ctx, m); err != nil {
		t.Fatalf("bad input returned an error (would be retried): %v", err)
	}
	bad, _ := h.s.ListBadReports(ctx, 10)
	if len(bad) != 1 || bad[0].FromCall != "N0CALL-1" || bad[0].Error == "" {
		t.Fatalf("bad reports = %+v", bad)
	}
	if sts, _ := h.s.ListStatuses(ctx); sts[0].BadReports != 1 {
		t.Fatalf("not counted against AS5: %+v", sts[0])
	}
}

func TestInvalidSourceCallsignDropped(t *testing.T) {
	h := newHarness(t)
	for _, src := range []string{"<SCRIPT", "=1+1", "BAD CALL", "", "TOOLONGCALL1", "N0CALL-99"} {
		m := h.gw.Inbound(src, "RC1 R AS5 1 @1300 1/00")
		if err := h.e.HandleInbound(ctx, m); err != nil {
			t.Fatalf("source %q: %v", src, err)
		}
	}
	if got, _ := h.s.EffectiveEntries(ctx, store.EntryFilter{}); len(got) != 0 {
		t.Fatalf("stored entries from invalid sources: %+v", got)
	}
	if bad, _ := h.s.ListBadReports(ctx, 10); len(bad) != 0 {
		t.Fatalf("invalid sources stored as bad reports: %+v", bad)
	}
}

func TestOtherTypesIgnoredAtHQ(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 G AS5 1")
	h.receive("N0CALL-1", "RC1 P AS5 3 1/5") // link check: phase 12
	if sts, _ := h.s.ListStatuses(ctx); len(sts) != 0 {
		t.Fatalf("statuses = %+v", sts)
	}
	if err := h.e.HandleOutbound(ctx, graywolf.Message{ID: 1, Status: graywolf.StatusAcked}); err != nil {
		t.Fatal(err)
	}
}

func TestRequestsGapAfterGrace(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 1 @1300 1/00")
	h.receive("N0CALL-1", "RC1 R AS5 3 @1300 3/00")
	h.tick()
	h.ft.Advance(89 * time.Second)
	h.tick()
	if n := len(h.gaps()); n != 0 {
		t.Fatalf("gap requested before gap_grace_sec: %d", n)
	}
	h.ft.Advance(time.Second)
	h.tick()
	got := h.gaps()
	if len(got) != 1 || got[0].Text != "RC1 G AS5 2" || got[0].To != "N0CALL-1" {
		t.Fatalf("gaps = %+v", got)
	}
	if known, _ := h.s.KnownGWRow(ctx, got[0].ID); !known {
		t.Error("gap request row not recorded for cleanup")
	}
}

func TestGapGoesToExpectedCallWhenSet(t *testing.T) {
	h := newHarness(t)
	if err := h.s.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Aid 5", ExpectedCall: "N0CALL-8"}); err != nil {
		t.Fatal(err)
	}
	h.receive("N0CALL-6", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.ft.Advance(90 * time.Second)
	h.tick()
	if got := h.gaps(); len(got) != 1 || got[0].To != "N0CALL-8" {
		t.Fatalf("gaps = %+v, want sent to the expected call", got)
	}
}

func TestGapBackoffGiveUpAndRecovery(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00") // seq 1 never arrives
	h.tick()
	h.run(6 * time.Hour)
	if n := len(h.gaps()); n != maxGapAttempts {
		t.Fatalf("requested %d times, want exactly %d then give up", n, maxGapAttempts)
	}
	if got := h.e.unrecoverable("AS5"); !slices.Equal(got, []uint32{1}) {
		t.Fatalf("unrecoverable = %v", got)
	}
	h.receive("N0CALL-1", "RC1 R AS5 1 @1300 1/00")
	h.tick()
	if got := h.e.unrecoverable("AS5"); len(got) != 0 {
		t.Fatalf("unrecoverable after arrival = %v", got)
	}
}

func TestGapFromHeartbeatTail(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 1 @1300 1/00")
	h.receive("N0CALL-1", "RC1 H AS5 2 130000") // batch 2 was sent but lost
	h.tick()
	h.ft.Advance(90 * time.Second)
	h.tick()
	if got := h.gaps(); len(got) != 1 || got[0].Text != "RC1 G AS5 2" {
		t.Fatalf("gaps = %+v", got)
	}
}

func TestGapRequestsRotate(t *testing.T) {
	h := newHarness(t)
	for seq := 2; seq <= 80; seq += 2 { // 40 isolated gaps
		h.receive("N0CALL-1", fmt.Sprintf("RC1 R AS5 %d @1300 %d/00", seq, seq))
	}
	h.tick()
	asked := map[uint32]bool{}
	for i := 0; i < 20 && len(asked) < 40; i++ {
		h.ft.Advance(90 * time.Second)
		h.tick()
		for _, m := range h.gaps() {
			g, err := wire.Decode(m.Text)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range g.(*wire.GapRequest).Seqs {
				asked[s] = true
			}
		}
	}
	if len(asked) != 40 {
		t.Fatalf("only %d of 40 missing seqs were ever requested", len(asked))
	}
}

func TestOneGapRequestPerTick(t *testing.T) {
	h := newHarness(t)
	for _, cp := range []string{"AS1", "AS2", "AS3"} {
		h.receive("KK7"+cp, "RC1 R "+cp+" 2 @1300 2/00")
	}
	h.tick()
	h.ft.Advance(90 * time.Second)
	for i := 1; i <= 3; i++ {
		h.tick()
		if n := len(h.gaps()); n != i {
			t.Fatalf("after tick %d: %d gap requests, want %d", i, n, i)
		}
		h.ft.Advance(time.Second)
	}
}

func TestGapSendFailureRetriesNextTick(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.ft.Advance(90 * time.Second)
	h.gw.FailNextSend(errors.New("graywolf down"))
	if err := h.e.Tick(ctx, h.cfg); err == nil {
		t.Fatal("expected error")
	}
	h.ft.Advance(time.Second)
	h.tick()
	if n := len(h.gaps()); n != 0 {
		t.Fatalf("retried at once (%d); want a backoff", n)
	}
	h.ft.Advance(sendFailBackoff)
	h.tick()
	if n := len(h.gaps()); n != 1 {
		t.Fatalf("gaps = %d, want the retry after the backoff", n)
	}
}

func TestGapBackoffSchedule(t *testing.T) {
	grace := 90 * time.Second
	if got := gapBackoff(1, grace); got != 180*time.Second {
		t.Errorf("first repeat = %v", got)
	}
	if got := gapBackoff(5, grace); got != gapRepeatFloor {
		t.Errorf("later repeat = %v", got)
	}
	if got := gapBackoff(5, 10*time.Minute); got != 20*time.Minute {
		t.Errorf("long grace repeat = %v", got)
	}
}

func TestRearm(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.run(6 * time.Hour) // gives up on seq 1
	n, err := h.e.Rearm(ctx, h.cfg, "AS5")
	if err != nil || n != 1 {
		t.Fatalf("Rearm = %d, %v", n, err)
	}
	before := len(h.gaps())
	h.tick()
	if len(h.gaps()) != before+1 {
		t.Fatal("re-armed seq not requested on the next tick")
	}
	if _, err := h.e.Rearm(ctx, h.cfg, "AS5"); !errors.Is(err, ErrTooSoon) {
		t.Fatalf("second Rearm err = %v, want ErrTooSoon", err)
	}
	if _, err := h.e.Rearm(ctx, h.cfg, "NOPE"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown cp err = %v", err)
	}
}

func TestHealth(t *testing.T) {
	h := newHarness(t)
	_ = h.s.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Aid 5", CourseOrder: 2, ExpectedCall: "N0CALL-1"})
	_ = h.s.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS9", Name: "Aid 9", CourseOrder: 3}) // never heard
	h.receive("SPOOF", "RC1 R AS5 2 @1300 2/00")                                                 // wrong sender
	h.receive("N0CALL-2", "RC1 R ZZ1 1 @1300 7/00")                                              // not on the course list
	h.tick()

	got, err := h.e.Health(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byCode := map[string]CheckpointHealth{}
	var order []string
	for _, c := range got {
		byCode[c.CPCode] = c
		order = append(order, c.CPCode)
	}
	if !slices.Equal(order, []string{"AS5", "AS9", "ZZ1"}) {
		t.Fatalf("order = %v, want course order then unknown codes", order)
	}
	as5 := byCode["AS5"]
	if !as5.SenderMismatch || as5.Name != "Aid 5" || !slices.Equal(as5.Missing, []uint32{1}) {
		t.Errorf("AS5 = %+v", as5)
	}
	if as9 := byCode["AS9"]; as9.LastHeardAt != nil || !as9.Defined {
		t.Errorf("AS9 = %+v, want defined but never heard", as9)
	}
	if zz := byCode["ZZ1"]; zz.Defined || zz.SenderMismatch {
		t.Errorf("ZZ1 = %+v, want undefined (heard only)", zz)
	}
}

func TestTurnsOffGraywolfRetriesBeforeGapRequest(t *testing.T) {
	h := newHarness(t)
	e, _ := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(h.ft.Now, nil), Now: h.ft.Now,
		Peers: peers.NewEnsurer(h.gw, h.s, h.ft.Now)})
	h.e = e
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.ft.Advance(90 * time.Second)
	h.tick()
	if p, _ := h.gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-1"); p.WaitForAck {
		t.Fatal("graywolf retries still on for the checkpoint after a gap request")
	}
}

func TestRunIdleUnlessHQ(t *testing.T) {
	h := newHarness(t)
	e, _ := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(nil, nil), Interval: 5 * time.Millisecond})
	cp := store.DefaultSettings()
	cp.Role, cp.CheckpointCode, cp.HQCall = store.RoleCheckpoint, "AS5", "N0CALL-10"
	if _, err := h.s.SaveSettings(ctx, cp); err != nil {
		t.Fatal(err)
	}
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	runCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := e.Run(runCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
	if n := len(h.gaps()); n != 0 {
		t.Fatalf("checkpoint node's HQ engine sent %d gap requests", n)
	}
}

func TestRunTicksAsHQ(t *testing.T) {
	h := newHarness(t)
	// A clock that jumps a minute per reading, so grace periods pass
	// within a few real ticks.
	var mu sync.Mutex
	fast := t0
	now := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		fast = fast.Add(time.Minute)
		return fast
	}
	e, _ := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(now, nil), Now: now, Interval: 5 * time.Millisecond})
	if _, err := h.s.SaveSettings(ctx, hqSettings()); err != nil {
		t.Fatal(err)
	}
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00") // seq 1 missing
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- e.Run(runCtx) }()
	deadline := time.Now().Add(3 * time.Second)
	for len(h.gw.TransmissionsWithPrefix("RC1 G ")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no gap request from the running HQ engine")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if gaps := h.gw.TransmissionsWithPrefix("RC1 G "); !strings.HasSuffix(gaps[0].Text, " 1") {
		t.Fatalf("gap = %q", gaps[0].Text)
	}
	cancel()
	<-done
}

func TestSendFailureDoesNotStarveOtherCheckpoints(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-4", "RC1 R AS1 2 @1300 2/00")
	h.receive("N0CALL-5", "RC1 R AS2 2 @1300 2/00")
	h.tick()
	h.ft.Advance(90 * time.Second)
	h.gw.FailNextSend(errors.New("graywolf down")) // AS1's request fails
	_ = h.e.Tick(ctx, h.cfg)
	h.ft.Advance(time.Second)
	h.tick()
	if got := h.gaps(); len(got) != 1 || got[0].To != "N0CALL-5" {
		t.Fatalf("gaps = %+v, want AS2 served while AS1 backs off", got)
	}
}

func TestOnlyListedCheckpointsAreChasedOnceAListExists(t *testing.T) {
	h := newHarness(t)
	_ = h.s.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Aid 5"})
	h.receive("SPOOF1", "RC1 R ZZ1 2 @1300 2/00") // phantom code
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.run(10 * time.Minute)
	for _, g := range h.gaps() {
		if strings.Contains(g.Text, "ZZ1") || g.To == "SPOOF1" {
			t.Fatalf("gap request for a phantom checkpoint: %+v", g)
		}
	}
	if len(h.gaps()) == 0 {
		t.Fatal("listed checkpoint AS5 not chased")
	}
}

func TestResetClearsGapState(t *testing.T) {
	h := newHarness(t)
	h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
	h.tick()
	h.run(6 * time.Hour)
	if len(h.e.unrecoverable("AS5")) != 1 {
		t.Fatal("setup: seq 1 should be given up")
	}
	_, _ = h.e.Rearm(ctx, h.cfg, "AS5")
	h.e.Reset()
	if len(h.e.unrecoverable("AS5")) != 0 {
		t.Fatal("given-up state survived Reset")
	}
	if _, err := h.e.Rearm(ctx, h.cfg, "AS5"); errors.Is(err, ErrTooSoon) {
		t.Fatal("re-request rate limit survived Reset")
	}
}

func TestRunGapRequestsOnlyWhileRacingOrComplete(t *testing.T) {
	for _, state := range []string{store.RaceSetup, store.RaceActive, store.RaceComplete} {
		h := newHarness(t)
		var mu sync.Mutex
		fast := t0
		now := func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			fast = fast.Add(time.Minute)
			return fast
		}
		e, _ := New(Config{Store: h.s, Graywolf: h.gw, Clock: raceclock.NewClock(now, nil), Now: now, Interval: 5 * time.Millisecond})
		cfg := hqSettings()
		cfg.RaceState = state
		if _, err := h.s.SaveSettings(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		h.receive("N0CALL-1", "RC1 R AS5 2 @1300 2/00")
		runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_ = e.Run(runCtx)
		cancel()
		got := len(h.gaps()) > 0
		if want := state != store.RaceSetup; got != want {
			t.Errorf("%s: gap requests sent = %v, want %v", state, got, want)
		}
	}
}
