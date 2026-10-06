package sim

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

// Soak (spec 9, phase 12c): a 12-hour race, 500 runners through 8
// checkpoints, 20% loss with duplicates and reordering. It asserts
// exactly-once at HQ, a flat heap, and database growth proportional to
// the data. Opt-in (CB_SOAK=1): it simulates 12+ hours of 9 nodes.
func TestSoak(t *testing.T) {
	if os.Getenv("CB_SOAK") == "" {
		t.Skip("set CB_SOAK=1 for the 12-hour soak simulation")
	}
	const (
		runners  = 500
		cpCount  = 8
		raceSpan = 12 * time.Hour
		legMean  = 75 * time.Minute
	)
	s := newSim(t, 99, gwfake.Profile{Loss: 0.2, Dup: 0.1, MaxDelay: 4 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	var cps []*Node
	for i := range cpCount {
		cps = append(cps, addNode(t, s, fmt.Sprintf("N0CALL-%d", i+1), CheckpointSettings(fmt.Sprintf("AS%d", i+1), hqCall)))
	}

	// Each runner starts in a 30-minute wave and keeps a personal pace;
	// slow runners past the 12-hour cutoff drop out (DNF), as in a race.
	type passage struct {
		at  time.Duration
		cp  int
		bib store.Bib
	}
	rng := rand.New(rand.NewPCG(5, 5))
	var plan []passage
	for r := range runners {
		at := time.Duration(rng.Int64N(int64(30 * time.Minute)))
		pace := 0.75 + 0.5*rng.Float64()
		for c := range cpCount {
			at += time.Duration(float64(legMean) * pace * (0.9 + 0.2*rng.Float64()))
			if at > raceSpan {
				break
			}
			plan = append(plan, passage{at: at, cp: c, bib: store.Bib(r + 1)})
		}
	}
	slices.SortFunc(plan, func(a, b passage) int { return cmp.Compare(a.at, b.at) })
	t.Logf("%d passages planned", len(plan))

	heap := func() uint64 {
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	start := s.Clock.Now()
	var heaps []uint64
	next := 0
	for hour := 1; next < len(plan); hour++ {
		for s.Clock.Now().Sub(start) < time.Duration(hour)*time.Hour {
			for next < len(plan) && plan[next].at <= s.Clock.Now().Sub(start) {
				logBib(t, s, cps[plan[next].cp], plan[next].bib)
				next++
			}
			s.Step()
		}
		heaps = append(heaps, heap())
		t.Logf("hour %2d: %4d passages logged, heap %5.1f MB, %d frames on air", hour, next, float64(heaps[len(heaps)-1])/1e6, s.Radio.Sent())
	}
	took := runUntilConfirmed(t, s, 4*time.Hour, cps...)
	s.Run(15 * time.Minute)
	t.Logf("all confirmed %v after the last passage", took)
	assertExactlyOnce(t, hqNode, cps...)

	// Flat memory: once warmed up, the heap doesn't keep growing with
	// time. (The data itself is a few thousand rows.)
	if first, last := heaps[1], heaps[len(heaps)-1]; last > 2*first+16<<20 {
		t.Errorf("heap grew from %.1f MB (hour 2) to %.1f MB (hour %d)", float64(first)/1e6, float64(last)/1e6, len(heaps))
	}
	// Bounded DB growth: bytes per passage stay small at every node.
	for _, n := range append([]*Node{hqNode}, cps...) {
		size, err := n.Store.DBSize(ctx)
		if err != nil {
			t.Fatal(err)
		}
		per := float64(size) / float64(len(plan))
		t.Logf("%s database %.1f MB (%.0f bytes per passage, race-wide)", n.Call, float64(size)/1e6, per)
		if per > 4096 {
			t.Errorf("%s: %.0f bytes per passage", n.Call, per)
		}
	}
}
