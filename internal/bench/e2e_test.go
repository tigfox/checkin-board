package bench

import (
	"context"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
	"checkin-board/internal/testnode"
)

const benchPassword = "bench-admin-password"

func startNode(ctx context.Context, t *testing.T, radio *gwfake.Radio, call string, cfg store.Settings) string {
	t.Helper()
	return testnode.Start(t, testnode.Options{Call: call, Password: benchPassword, Settings: &cfg, Radio: radio, Run: true}).URL
}

func cpSettings(code string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceState = store.RoleCheckpoint, code, "HQ-1", store.RaceActive
	c.FlushAfterSec = 5
	return c
}

// A real-time bench over a lossy channel: every passage reaches HQ
// exactly once through the nodes' own web APIs.
func TestBenchEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("real-time bench (up to a few minutes)")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	radio := gwfake.NewRadio(3, gwfake.Profile{Loss: 0.1, Dup: 0.05, MaxDelay: 300 * time.Millisecond}, time.Now)
	go func() {
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				radio.Deliver()
			}
		}
	}()
	hqCfg := store.DefaultSettings()
	hqCfg.Role, hqCfg.HQLocalCodes, hqCfg.RaceState = store.RoleHQ, "START,FIN", store.RaceActive
	hqURL := startNode(ctx, t, radio, "HQ-1", hqCfg)
	cp1 := startNode(ctx, t, radio, "CP-1", cpSettings("AS1"))
	cp2 := startNode(ctx, t, radio, "CP-2", cpSettings("AS2"))

	var out strings.Builder
	rep, err := Run(ctx, Config{
		HQ: hqURL, Checkpoints: []string{cp1, cp2}, Password: benchPassword,
		Runners: 20, FirstBib: 9001, Duration: 4 * time.Second, Settle: 4 * time.Minute,
		PollEvery: time.Second, Seed: 1, Out: &out,
	})
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	t.Logf("%+v\n%s", rep, out.String())
	if !rep.OK() || rep.Expected != 40 || len(rep.PerCP) != 2 {
		t.Fatalf("report = %+v\n%s", rep, out.String())
	}

	// A second run over the same bibs is refused before logging anything.
	_, err = Run(ctx, Config{HQ: hqURL, Checkpoints: []string{cp1}, Password: benchPassword,
		Runners: 5, FirstBib: 9001, Duration: time.Second, Settle: time.Second})
	if err == nil || !strings.Contains(err.Error(), "already has bib") {
		t.Fatalf("rerun: err = %v", err)
	}
}

func TestBenchSetupChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	radio := gwfake.NewRadio(4, gwfake.Profile{}, time.Now)
	hqCfg := store.DefaultSettings()
	hqCfg.Role, hqCfg.HQLocalCodes, hqCfg.RaceState = store.RoleHQ, "START", store.RaceActive
	hqURL := startNode(ctx, t, radio, "HQ-1", hqCfg)
	closed := cpSettings("AS1")
	closed.RaceState = store.RaceSetup
	cpClosed := startNode(ctx, t, radio, "CP-1", closed)

	for name, c := range map[string]Config{
		"wrong password":           {HQ: hqURL, Checkpoints: []string{cpClosed}, Password: "nope"},
		"checkpoint not open":      {HQ: hqURL, Checkpoints: []string{cpClosed}, Password: benchPassword},
		"HQ given as a checkpoint": {HQ: hqURL, Checkpoints: []string{hqURL}, Password: benchPassword},
		"no checkpoints":           {HQ: hqURL, Password: benchPassword},
	} {
		c.Runners, c.FirstBib, c.Duration = 5, 9001, time.Second
		if _, err := Run(ctx, c); err == nil || !strings.Contains(err.Error(), "bench: setup") {
			t.Errorf("%s: err = %v, want a setup error", name, err)
		}
	}
}
