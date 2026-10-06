package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/inbox"
	"checkin-board/internal/store"
)

var ctx = context.Background()

type recorder struct{ in, out int }

func (r *recorder) HandleInbound(context.Context, graywolf.Message) error  { r.in++; return nil }
func (r *recorder) HandleOutbound(context.Context, graywolf.Message) error { r.out++; return nil }

type fixedSettings struct {
	s   store.Settings
	err error
}

func (f fixedSettings) GetSettings(context.Context) (store.Settings, error) { return f.s, f.err }

func TestDispatcherRoutesByRole(t *testing.T) {
	cp, hq := &recorder{}, &recorder{}
	for _, tc := range []struct {
		role     string
		cpIn, hq int
	}{
		{store.RoleCheckpoint, 1, 0},
		{store.RoleHQ, 0, 1},
		{store.RoleUnset, 0, 0},
	} {
		*cp, *hq = recorder{}, recorder{}
		d := NewDispatcher(fixedSettings{s: store.Settings{Role: tc.role}}, cp, hq)
		errIn := d.HandleInbound(ctx, graywolf.Message{})
		errOut := d.HandleOutbound(ctx, graywolf.Message{})
		if wantNotReady := tc.role == store.RoleUnset; errors.Is(errIn, inbox.ErrNotReady) != wantNotReady || errors.Is(errOut, inbox.ErrNotReady) != wantNotReady {
			t.Errorf("role %q: errs = %v, %v", tc.role, errIn, errOut)
		}
		if cp.in != tc.cpIn || cp.out != tc.cpIn || hq.in != tc.hq || hq.out != tc.hq {
			t.Errorf("role %q: cp=%+v hq=%+v", tc.role, cp, hq)
		}
	}
	boom := errors.New("db down")
	d := NewDispatcher(fixedSettings{err: boom}, cp, hq)
	if err := d.HandleInbound(ctx, graywolf.Message{}); !errors.Is(err, boom) {
		t.Errorf("inbound err = %v", err)
	}
	if err := d.HandleOutbound(ctx, graywolf.Message{}); !errors.Is(err, boom) {
		t.Errorf("outbound err = %v", err)
	}
}

func newApp(t *testing.T, gw *gwfake.Station, settings store.Settings) (*App, *store.Store) {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SaveSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	a, err := New(Config{Store: st, Graywolf: gw})
	if err != nil {
		t.Fatal(err)
	}
	return a, st
}

func runApp(t *testing.T, a *App) {
	t.Helper()
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestHQNodeIngestsFromGraywolf(t *testing.T) {
	gw := gwfake.New("N0HQ")
	cfg := store.DefaultSettings()
	cfg.Role, cfg.HQLocalCodes = store.RoleHQ, "FIN"
	a, st := newApp(t, gw, cfg)
	runApp(t, a)

	gw.Inbound("K1CP", "RC1 R AS5 1 @1300 101/05 102/30")
	waitFor(t, "report ingested", func() bool {
		got, _ := st.EffectiveEntries(ctx, store.EntryFilter{})
		return len(got) == 2
	})
	waitFor(t, "row marked read", func() bool {
		rows := gw.Rows()
		return len(rows) == 1 && !rows[0].Unread
	})
}

func TestCheckpointNodeSendsAndConfirms(t *testing.T) {
	gw := gwfake.New("K1CP")
	cfg := store.DefaultSettings()
	cfg.Role, cfg.CheckpointCode, cfg.HQCall, cfg.RaceState = store.RoleCheckpoint, "AS5", "N0HQ", store.RaceActive
	a, st := newApp(t, gw, cfg)
	runApp(t, a)

	// A full frame's worth flushes at once instead of after 20 s.
	for bib := store.Bib(1001); bib <= 1004; bib++ {
		if _, err := st.LogLocal(ctx, "AS5", bib, time.Now(), true); err != nil {
			t.Fatal(err)
		}
	}
	var batchRow uint64
	waitFor(t, "batch sent", func() bool {
		if r := gw.TransmissionsWithPrefix("RC1 R "); len(r) > 0 {
			batchRow = r[0].ID
			return true
		}
		return false
	})
	if p, _ := gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ"); p.WaitForAck {
		t.Error("graywolf retries not turned off for HQ")
	}

	gw.Ack(batchRow) // HQ's graywolf ACKed it; the event reaches the reader
	waitFor(t, "entries confirmed", func() bool {
		ob, _ := st.OutboxStats(ctx)
		return ob.Unconfirmed == 0 && ob.PendingBatches == 0
	})
	if a.Checkpoint.LastContact().IsZero() {
		t.Error("HQ contact not recorded")
	}
}

func TestRefreshPrefsUpdatesEngines(t *testing.T) {
	gw := gwfake.New("N0HQ")
	a, _ := newApp(t, gw, store.DefaultSettings())
	gw.SetMaxText(150)
	if err := a.RefreshPrefs(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestUnconfiguredHQKeepsReportsUntilConfigured(t *testing.T) {
	gw := gwfake.New("N0HQ")
	a, st := newApp(t, gw, store.DefaultSettings()) // no role yet
	gw.Inbound("K1CP", "RC1 R AS5 1 @1300 101/05")
	runApp(t, a)
	time.Sleep(300 * time.Millisecond)
	if got, _ := st.EffectiveEntries(ctx, store.EntryFilter{}); len(got) != 0 {
		t.Fatalf("ingested before configured: %+v", got)
	}
	cfg := store.DefaultSettings()
	cfg.Role, cfg.HQLocalCodes = store.RoleHQ, "FIN"
	if _, err := st.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	a.Inbox.Kick()
	waitFor(t, "report ingested after configuring HQ", func() bool {
		got, _ := st.EffectiveEntries(ctx, store.EntryFilter{})
		return len(got) == 1
	})
}

func TestStartingPointSavedOnceAndReadsEarlierRows(t *testing.T) {
	gw := gwfake.New("N0HQ")
	cfg := store.DefaultSettings()
	cfg.Role, cfg.HQLocalCodes = store.RoleHQ, "FIN"
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_, _ = st.SaveSettings(ctx, cfg)
	first := time.Now().Add(-time.Hour)
	if err := st.EnsureInboxSince(ctx, first); err != nil { // the node's first run
		t.Fatal(err)
	}
	gw.Inbound("K1CP", "RC1 R AS5 1 @1300 101/05") // arrives while the app is down
	// Restart: StartedAt is now, but the first run's starting point holds.
	a, err := New(Config{Store: st, Graywolf: gw, StartedAt: time.Now().Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	runApp(t, a)
	waitFor(t, "report from before the restart ingested", func() bool {
		got, _ := st.EffectiveEntries(ctx, store.EntryFilter{})
		return len(got) == 1
	})
}
