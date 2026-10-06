package sim

import (
	"fmt"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
)

// runLinkCheck runs one checkpoint → HQ link check in a fresh sim.
func runLinkCheck(t *testing.T, seed uint64, loss float64) store.LinkCheck {
	t.Helper()
	s := newSim(t, seed, gwfake.Profile{Loss: loss, MaxDelay: 3 * time.Second})
	setup := func(c store.Settings) store.Settings { c.RaceState = store.RaceSetup; return c }
	addNode(t, s, hqCall, setup(HQSettings()))
	cp := addNode(t, s, "N0CALL-7", setup(CheckpointSettings("AS1", hqCall)))
	req, err := linkcheck.Request(ctx, cp.Store, linkcheck.Req{}, s.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	for range 10 * 60 {
		s.Step()
		c, err := cp.Store.GetLinkCheck(ctx, req.ID)
		if err != nil {
			t.Fatal(err)
		}
		if c.State == store.LinkCheckDone {
			return c
		}
	}
	t.Fatalf("seed %d loss %.0f%%: link check never finished", seed, loss*100)
	return store.LinkCheck{}
}

// The verdict tracks the channel: a clean link passes, a dead one fails,
// and moderate loss lands in between (spec 4.8.4).
func TestLinkCheckVerdictTracksLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	const seeds = 20
	for _, tc := range []struct {
		loss             float64
		minPass, maxPass int
		minFail, maxFail int
	}{
		{0, seeds, seeds, 0, 0},
		{0.1, 8, seeds, 0, 2},
		{0.25, 0, seeds, 0, 8},
		// 60% each way with five probes sits on the "half each way"
		// line: MARGINAL or FAIL, almost never PASS.
		{0.4, 0, 3, 5, seeds - 3},
		{0.8, 0, 0, 15, seeds},
		{1, 0, 0, seeds, seeds},
	} {
		got := map[string]int{}
		for seed := range uint64(seeds) {
			got[runLinkCheck(t, seed+1, tc.loss).Verdict]++
		}
		t.Logf("loss %3.0f%%: %v", tc.loss*100, got)
		if p, f := got[linkcheck.Pass], got[linkcheck.Fail]; p < tc.minPass || p > tc.maxPass || f < tc.minFail || f > tc.maxFail {
			t.Errorf("loss %.0f%%: %v, want PASS %d..%d, FAIL %d..%d", tc.loss*100, got, tc.minPass, tc.maxPass, tc.minFail, tc.maxFail)
		}
	}
}

// A confirmed link check during the race doesn't disturb delivery.
func TestLinkCheckDuringRaceKeepsExactlyOnce(t *testing.T) {
	s := newSim(t, 3, gwfake.Profile{Loss: 0.2, MaxDelay: 3 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	if _, err := linkcheck.Request(ctx, cp.Store, linkcheck.Req{}, s.Clock.Now()); err == nil {
		t.Fatal("link check during the race ran without a confirm")
	}
	req, err := linkcheck.Request(ctx, cp.Store, linkcheck.Req{Confirm: true}, s.Clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	for bib := store.Bib(1); bib <= 40; bib++ {
		logBib(t, s, cp, bib)
		s.Run(3 * time.Second)
	}
	runUntilConfirmed(t, s, time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
	c, _ := cp.Store.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckDone {
		t.Fatalf("link check = %+v", c)
	}
	t.Logf("link check during race: %s (uplink %d, round trip %d)", c.Verdict, c.Uplink, c.RoundTrip)
	if hb := fmt.Sprint(c.Verdict); hb == "" {
		t.Fatal("no verdict")
	}
}
