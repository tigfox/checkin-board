package inbox

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestReader(t *testing.T, gw *fakeGW, st *store.Store, d Dispatcher) *Reader {
	t.Helper()
	r, err := New(Config{
		Graywolf:   gw,
		Store:      st,
		Dispatcher: d,
		Backstop:   time.Hour, // tests trigger catch-ups explicitly unless they shorten it
		Backoff:    []time.Duration{10 * time.Millisecond, 20 * time.Millisecond},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

var ctx = context.Background()

func TestNewValidates(t *testing.T) {
	gw, st, d := newFakeGW(), newTestStore(t), newRecorder()
	bad := []Config{
		{Store: st, Dispatcher: d},
		{Graywolf: gw, Dispatcher: d},
		{Graywolf: gw, Store: st},
	}
	for i, cfg := range bad {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %d: expected error", i)
		}
	}
}

func TestCatchUpDispatchesRaceTrafficOnce(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)

	gw.inbound(1, "RC1 H AS5 0 120000")
	gw.inbound(2, "hello from a ham") // operator traffic: ignored
	tactical := graywolf.Message{ID: 3, Direction: "in", ThreadKind: graywolf.ThreadKindTactical, Text: "RC1 H AS5 0 120000"}
	gw.put(tactical) // race text in a tactical thread: ignored
	unread := graywolf.Message{ID: 4, Direction: "in", ThreadKind: graywolf.ThreadKindDM, Text: "RC1 R AS5 1 @1200 101/05", Unread: true}
	gw.put(unread)
	gw.put(graywolf.Message{ID: 5, Direction: "out", ThreadKind: graywolf.ThreadKindDM, Text: "RC1 R 3 1 @1200 7/01", Status: graywolf.StatusSentRF})
	gw.put(graywolf.Message{ID: 6, Direction: "out", ThreadKind: graywolf.ThreadKindDM, Text: "73", Status: graywolf.StatusAcked})

	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{1, 4}) {
		t.Fatalf("inbound = %v, want [1 4]", got)
	}
	if got := rec.outboundSeen(); !slices.Equal(got, []string{"5:sent_rf"}) {
		t.Fatalf("outbound = %v", got)
	}
	if got := gw.readIDs(); !slices.Contains(got, 4) {
		t.Fatalf("unread race row not marked read: %v", got)
	}
	for _, id := range []uint64{1, 4} {
		if known, _ := st.KnownGWRow(ctx, id); !known {
			t.Errorf("row %d not recorded", id)
		}
	}
	cur, _ := st.InboxCursor(ctx)
	if cur == "" {
		t.Fatal("cursor not saved")
	}

	// Mark-read moved row 4 to the end of the feed, and row 5 got ACKed.
	gw.put(graywolf.Message{ID: 5, Direction: "out", ThreadKind: graywolf.ThreadKindDM, Text: "RC1 R 3 1 @1200 7/01", Status: graywolf.StatusAcked})
	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); len(got) != 2 {
		t.Fatalf("inbound re-dispatched: %v", got)
	}
	if got := rec.outboundSeen(); !slices.Equal(got, []string{"5:sent_rf", "5:acked"}) {
		t.Fatalf("outbound = %v, want the ACK too", got)
	}
}

func TestDispatchErrorRetriesWithoutLosingRows(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	for id := uint64(1); id <= 4; id++ {
		gw.inbound(id, "RC1 H AS5 0 120000")
	}
	rec.failIn[3] = 1 // page 2 fails once

	if err := r.CatchUp(ctx); !errors.Is(err, errDispatch) {
		t.Fatalf("err = %v, want dispatch error", err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("after failure = %v", got)
	}
	if st := r.Status(); st.CatchUpError == "" {
		t.Error("Status.CatchUpError empty after a failed catch-up")
	}
	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{1, 2, 3, 4}) {
		t.Fatalf("after retry = %v, want each row exactly once", got)
	}
	if st := r.Status(); st.CatchUpError != "" || st.LastCatchUpAt.IsZero() {
		t.Errorf("status after success = %+v", st)
	}
}

func TestCursorSurvivesRestart(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	gw.inbound(1, "RC1 H AS5 0 120000")
	if err := newTestReader(t, gw, st, rec).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	gw.inbound(2, "RC1 H AS5 1 120500")
	before := gw.listCalls()

	// A new Reader (process restart) resumes from the saved cursor.
	rec2 := newRecorder()
	if err := newTestReader(t, gw, st, rec2).CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec2.inboundIDs(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("after restart = %v, want only the new row", got)
	}
	if gw.listCalls() != before+1 {
		t.Errorf("list calls = %d", gw.listCalls()-before)
	}
}

func TestStartingPointUsedOnlyWithoutCursor(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	gw.inbound(100, "RC1 H OLD 0 120000") // created before the node's first run
	gw.inbound(300, "RC1 H AS5 0 120000")
	if err := st.EnsureInboxSince(ctx, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	r := newTestReader(t, gw, st, rec)
	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{300}) {
		t.Fatalf("inbound = %v, want only rows since the starting point", got)
	}
	if len(gw.sinces) != 1 || !gw.sinces[0].Equal(time.Unix(200, 0)) {
		t.Fatalf("since params = %v", gw.sinces)
	}
}

func TestNotReadyHoldsWithoutSkipping(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.inbound(1, "RC1 R AS5 1 @1300 1/00")
	gw.inbound(2, "RC1 R AS5 2 @1300 2/00")
	rec.notReady = true // e.g. HQ not configured yet
	for range maxRowFailures + 2 {
		if err := r.CatchUp(ctx); !errors.Is(err, ErrNotReady) {
			t.Fatalf("err = %v, want ErrNotReady", err)
		}
	}
	if s := r.Status(); s.SkippedRows != 0 {
		t.Fatalf("rows skipped while not ready: %+v", s)
	}
	if known, _ := st.KnownGWRow(ctx, 1); known {
		t.Fatal("row recorded as handled while not ready")
	}
	rec.notReady = false // operator sets the role
	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("inbound = %v, want both once ready", got)
	}
}

func TestMarkReadFailureIsNotFatal(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.readErr = errors.New("graywolf hiccup")
	gw.put(graywolf.Message{ID: 1, Direction: "in", ThreadKind: graywolf.ThreadKindDM, Text: "RC1 H AS5 0 120000", Unread: true})
	if err := r.CatchUp(ctx); err != nil {
		t.Fatalf("mark-read failure surfaced: %v", err)
	}
	if known, _ := st.KnownGWRow(ctx, 1); !known {
		t.Fatal("row not recorded despite successful dispatch")
	}
}

func TestListErrorReportedInStatus(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.listErr = graywolf.ErrAuth
	if err := r.CatchUp(ctx); !errors.Is(err, graywolf.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
	if st := r.Status(); !st.AuthFailed || st.CatchUpError == "" {
		t.Fatalf("status = %+v, want auth failure flagged", st)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunEventTriggersCatchUp(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()

	<-gw.streams // connected
	waitFor(t, "connected status", func() bool { return r.Status().Connected })
	gw.inbound(1, "RC1 H AS5 0 120000")
	gw.events <- graywolf.Event{Type: graywolf.EventReceived, Change: graywolf.MessageChange{ID: 1}}
	waitFor(t, "dispatch of row 1", func() bool { return len(rec.inboundIDs()) == 1 })
	if r.Status().LastEventAt.IsZero() {
		t.Error("LastEventAt not set")
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
}

func TestRunReconnectsAndCatchesUp(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()

	<-gw.streams
	// graywolf restarts: the stream drops, and a row arrives meanwhile
	// with no event ever sent for it.
	gw.inbound(1, "RC1 H AS5 0 120000")
	gw.mu.Lock()
	gw.streamErr = errors.New("connection reset")
	gw.mu.Unlock()
	close(gw.events)

	<-gw.streams // reconnect attempt (fails)
	waitFor(t, "disconnected status", func() bool { return !r.Status().Connected })
	gw.mu.Lock()
	gw.streamErr = nil
	gw.events = make(chan graywolf.Event, 16)
	gw.mu.Unlock()
	<-gw.streams // reconnected after backoff
	waitFor(t, "catch-up after reconnect", func() bool { return len(rec.inboundIDs()) == 1 })

	cancel()
	<-done
}

func TestRunBackstopCatchesSilentRows(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r, err := New(Config{Graywolf: gw, Store: st, Dispatcher: rec, Backstop: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	<-gw.streams

	gw.inbound(1, "RC1 H AS5 0 120000") // graywolf dropped the event
	waitFor(t, "backstop catch-up", func() bool { return len(rec.inboundIDs()) == 1 })
	cancel()
	<-done
}

func TestPoisonRowIsSkippedAfterRepeatedFailures(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.inbound(1, "RC1 H AS5 0 120000")
	gw.inbound(2, "RC1 H AS5 1 120500")
	rec.failIn[1] = 1000 // a dispatcher bug: always fails

	for i := 1; i < maxRowFailures; i++ {
		if err := r.CatchUp(ctx); err == nil {
			t.Fatalf("attempt %d: expected failure", i)
		}
	}
	if len(rec.inboundIDs()) != 0 {
		t.Fatal("row 2 dispatched while row 1 still blocks")
	}
	if err := r.CatchUp(ctx); err != nil {
		t.Fatalf("final attempt should skip row 1: %v", err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{2}) {
		t.Fatalf("inbound = %v, want the feed unblocked", got)
	}
	if s := r.Status(); s.SkippedRows != 1 || s.LastSkipped == "" {
		t.Fatalf("status = %+v", s)
	}
	if known, _ := st.KnownGWRow(ctx, 1); !known {
		t.Error("skipped row not recorded; it would be retried when it reappears")
	}
}

func TestPermanentErrorSkipsAtOnce(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.inbound(1, "RC1 H AS5 0 120000")
	gw.inbound(2, "RC1 H AS5 1 120500")
	rec.permIn[1] = true
	if err := r.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if got := rec.inboundIDs(); !slices.Equal(got, []uint64{2}) || r.Status().SkippedRows != 1 {
		t.Fatalf("inbound = %v, status = %+v", got, r.Status())
	}
}

func TestMarkReadRetriedOnNextCatchUp(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	r := newTestReader(t, gw, st, rec)
	gw.readErr = errors.New("busy")
	gw.put(graywolf.Message{ID: 1, Direction: "in", ThreadKind: graywolf.ThreadKindDM, Text: "RC1 H AS5 0 120000", Unread: true})
	_ = r.CatchUp(ctx)
	gw.mu.Lock()
	gw.readErr = nil
	gw.mu.Unlock()
	_ = r.CatchUp(ctx)
	if got := gw.readIDs(); len(got) != 2 || got[1] != 1 {
		t.Fatalf("mark-read calls = %v, want a retry of row 1", got)
	}
	_ = r.CatchUp(ctx)
	if got := gw.readIDs(); len(got) != 2 {
		t.Fatalf("mark-read retried after success: %v", got)
	}
}

func TestRunDrainsLargeBacklogWithoutWaiting(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	gw.maxPages = 1 // each catch-up handles one page of 2
	for id := uint64(1); id <= 7; id++ {
		gw.inbound(id, "RC1 H AS5 0 120000")
	}
	r := newTestReader(t, gw, st, rec) // backstop is an hour
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()
	waitFor(t, "full backlog", func() bool { return len(rec.inboundIDs()) == 7 })
	cancel()
	<-done
}

func TestConnectedOnlyAfterStreamOpens(t *testing.T) {
	gw, st, rec := newFakeGW(), newTestStore(t), newRecorder()
	gw.streamErr = errors.New("connection refused")
	r := newTestReader(t, gw, st, rec)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Run(runCtx) }()

	<-gw.streams
	<-gw.streams // two refused attempts
	if s := r.Status(); s.Connected || s.StreamError == "" {
		t.Fatalf("status during refused connects = %+v", s)
	}
	gw.mu.Lock()
	gw.streamErr = nil
	gw.mu.Unlock()
	waitFor(t, "connected", func() bool { s := r.Status(); return s.Connected && s.StreamError == "" })
	// A catch-up error doesn't touch the stream's state, and vice versa.
	gw.mu.Lock()
	gw.listErr = errors.New("list broke")
	gw.mu.Unlock()
	r.Kick()
	waitFor(t, "catch-up error", func() bool { return r.Status().CatchUpError != "" })
	if s := r.Status(); !s.Connected || s.StreamError != "" {
		t.Fatalf("stream state disturbed by catch-up error: %+v", s)
	}
	cancel()
	<-done
}
