package sim

import (
	"math"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

// shippedDrain is the original pkg/race design's measured average drain
// time per loss rate (spec 9b, "Shipped" column, 5 seeds), for context.
var shippedDrain = map[float64]time.Duration{
	0.1: 22 * time.Second,
	0.2: 37 * time.Second,
	0.3: 3*time.Minute + 2*time.Second,
	0.4: 16*time.Minute + 4*time.Second,
	0.5: 42*time.Minute + 41*time.Second,
}

// baselineDrain is this implementation's average drain over 100 seeds
// (2026-10-06, spec 9b). The acceptance check guards against
// regressions from it: the 5-seed original figures are too noisy to
// compare against (the tail-stall variance swamps them).
var baselineDrain = map[float64]time.Duration{
	0.1: 29 * time.Second,
	0.2: time.Minute + 47*time.Second,
	0.3: 4*time.Minute + 38*time.Second,
	0.4: 21 * time.Minute,
	0.5: time.Hour + 5*time.Minute + 28*time.Second,
}

// maxDeadLinkFramesPerHour is the original design's measured upper bound
// for airtime on a dead link (spec 9b).
const maxDeadLinkFramesPerHour = 42

// TestLatencyTable re-runs the spec 9b simulation: a 300-runner mass
// start (~20 bibs/min) through one aid station; every frame
// independently lost at the given rate, 10% duplicated, up to 3 s delay;
// CB_LATENCY_SEEDS seeds (default 30). It reports drain time (last bib
// to all confirmed) with a 95% confidence interval, frames per run, and
// dead-link airtime. It fails if a loss rate's drain is more than 25%
// slower than baselineDrain beyond the noise (lower CI bound above
// 1.25 × baseline), or if dead-link airtime exceeds the original
// design's bound. Run with CB_LATENCY_TABLE=1; a few minutes.
func TestLatencyTable(t *testing.T) {
	if os.Getenv("CB_LATENCY_TABLE") == "" {
		t.Skip("set CB_LATENCY_TABLE=1")
	}
	seeds := uint64(30)
	if v, err := strconv.ParseUint(os.Getenv("CB_LATENCY_SEEDS"), 10, 64); err == nil && v > 0 {
		seeds = v
	}
	for _, loss := range []float64{0.1, 0.2, 0.3, 0.4, 0.5} {
		var worst, total time.Duration
		var frames, deadFrames int
		var drains []float64
		for seed := uint64(1); seed <= seeds; seed++ {
			s := New(seed, gwfake.Profile{Loss: loss, Dup: 0.1, MaxDelay: 3 * time.Second})
			if _, err := s.AddNode(hqCall, HQSettings()); err != nil {
				t.Fatal(err)
			}
			cp, err := s.AddNode("KK7CP-7", CheckpointSettings("AS1", hqCall))
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewPCG(seed, 99))
			for bib := store.Bib(1); bib <= 300; bib++ {
				if _, err := s.LogBib(cp, bib); err != nil {
					t.Fatal(err)
				}
				s.Run(time.Duration(1+rng.IntN(5)) * time.Second)
			}
			d, err := s.RunUntilConfirmed(6*time.Hour, cp)
			if err != nil {
				t.Fatalf("loss %.0f%% seed %d: %v", loss*100, seed, err)
			}
			total += d
			worst = max(worst, d)
			drains = append(drains, d.Seconds())
			frames += s.Radio.Sent()

			// Dead-link airtime: an hour of total outage with batches
			// outstanding.
			for bib := store.Bib(301); bib <= 320; bib++ {
				if _, err := s.LogBib(cp, bib); err != nil {
					t.Fatal(err)
				}
			}
			s.Run(time.Minute)
			s.Radio.SetDown(true)
			before := s.Radio.Sent()
			s.Run(time.Hour)
			deadFrames += s.Radio.Sent() - before
			s.Close()
		}
		avg := total / time.Duration(seeds)
		ref := shippedDrain[loss]
		ci := ci95(drains)
		t.Logf("loss %2.0f%%: drain avg %8v ±%v (95%% CI) worst %8v (original %8v) | frames/run %d | dead-link frames/hour %d",
			loss*100, avg.Round(time.Second), ci.Round(time.Second), worst.Round(time.Second), ref, frames/int(seeds), deadFrames/int(seeds))
		if limit := baselineDrain[loss] * 5 / 4; avg-ci > limit {
			t.Errorf("loss %.0f%%: drain %v ±%v regressed past %v (baseline %v + 25%%)",
				loss*100, avg.Round(time.Second), ci.Round(time.Second), limit.Round(time.Second), baselineDrain[loss])
		}
		if per := deadFrames / int(seeds); per > maxDeadLinkFramesPerHour {
			t.Errorf("loss %.0f%%: %d dead-link frames/hour, want <= %d", loss*100, per, maxDeadLinkFramesPerHour)
		}
	}
}

// ci95 is the half-width of the 95% confidence interval of the mean.
func ci95(xs []float64) time.Duration {
	if len(xs) < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var ss float64
	for _, x := range xs {
		ss += (x - mean) * (x - mean)
	}
	sd := math.Sqrt(ss / float64(len(xs)-1))
	z := 1.96
	if len(xs) < 60 {
		z = 2.05 // Student's t for ~30 samples; the data is skewed, so err wide
	}
	return time.Duration(z * sd / math.Sqrt(float64(len(xs))) * float64(time.Second))
}
