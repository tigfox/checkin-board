package ops

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/checkpoint"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/hq"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
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

type env struct {
	t     *testing.T
	svc   *Service
	st    *store.Store
	gw    *gwfake.Station
	ft    *fakeTime
	clock *raceclock.Clock
	cp    *checkpoint.Engine
	hq    *hq.Engine
	dir   string
	cfg   store.Settings
}

func checkpointSettings() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceName = store.RoleCheckpoint, "AS5", "N0CALL-10", "Ridge 50K"
	return c
}

func hqSettings() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceName = store.RoleHQ, "START,FIN", "Ridge 50K"
	return c
}

// newEnv builds a Service on a file-backed store (so the trigger test
// can reach the same database) in a temp dir, in the given state.
func newEnv(t *testing.T, cfg store.Settings, state string) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "checkin-board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ft := &fakeTime{t: t0}
	st.SetClock(ft.Now)
	cfg.RaceState = state
	if _, err := st.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	gw := gwfake.New("N0CALL-1")
	gw.Now = ft.Now
	// Synced from a volunteer's browser (no disciplined OS clock), so a
	// reset's clearing of the sync is observable.
	clock := raceclock.NewClock(ft.Now, nil)
	if err := clock.Sync(t0, 0); err != nil {
		t.Fatal(err)
	}
	ens := peers.NewEnsurer(gw, st, ft.Now)
	cp, _ := checkpoint.New(checkpoint.Config{Store: st, Graywolf: gw, Clock: clock, Now: ft.Now, Peers: ens})
	hqe, _ := hq.New(hq.Config{Store: st, Graywolf: gw, Clock: clock, Now: ft.Now, Peers: ens})
	svc, err := New(Config{
		Store: st, Graywolf: gw, Clock: clock, Peers: ens, Checkpoint: cp, HQ: hqe,
		JournalPath: filepath.Join(dir, "race-journal.csv"), BackupDir: filepath.Join(dir, "backups"), Now: ft.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return &env{t: t, svc: svc, st: st, gw: gw, ft: ft, clock: clock, cp: cp, hq: hqe, dir: dir, cfg: cfg}
}

func (e *env) logBib(cp string, bib store.Bib) LogResult {
	e.t.Helper()
	r, err := e.svc.LogBib(ctx, cp, bib)
	if err != nil {
		e.t.Fatalf("LogBib(%s, %d): %v", cp, bib, err)
	}
	return r
}

func (e *env) state() string {
	e.t.Helper()
	c, err := e.st.GetSettings(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	return c.RaceState
}

func TestNewValidates(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("expected error")
	}
}
