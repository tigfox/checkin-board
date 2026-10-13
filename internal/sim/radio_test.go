package sim

import (
	"math/rand/v2"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/radiokind"
	"checkin-board/internal/store"
)

// Phase 17c: UV-K5 radio checkpoints in the same race as graywolf nodes.

func addRadioNode(t *testing.T, s *Sim, call string, cfg store.Settings) *Node {
	t.Helper()
	n, err := s.AddRadioNode(call, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// massStart logs n runners at cp, one every 1-3 s (about 30 a minute).
func massStart(t *testing.T, s *Sim, rng *rand.Rand, first store.Bib, n int, cps ...*Node) {
	t.Helper()
	for bib := first; bib < first+store.Bib(n); bib++ {
		for _, cp := range cps {
			logBib(t, s, cp, bib)
		}
		s.Run(time.Duration(1+rng.IntN(3)) * time.Second)
	}
}

// A graywolf HQ and one radio checkpoint: 100 runners in about 3 minutes
// over a lossy channel arrive exactly once.
func TestRadioCheckpointExactlyOnce(t *testing.T) {
	s := newSim(t, 11, gwfake.Profile{Loss: 0.2, Dup: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	massStart(t, s, rand.New(rand.NewPCG(11, 11)), 1, 100, cp)
	took := runUntilConfirmed(t, s, 2*time.Hour, cp)
	t.Logf("radio checkpoint: 100 runners confirmed %v after the last; %d frames on air", took, s.Radio.Sent())
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
	if took > 15*time.Minute {
		t.Errorf("drain took %v after the last bib, want under 15m at 20%% loss", took)
	}
}

// Radio and graywolf checkpoints side by side, reporting to one HQ.
func TestMixedCheckpoints(t *testing.T) {
	s := newSim(t, 12, gwfake.Profile{Loss: 0.1, Dup: 0.05, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	radioCP := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	gwCP := addNode(t, s, "N0CALL-8", CheckpointSettings("AS2", hqCall))
	massStart(t, s, rand.New(rand.NewPCG(12, 12)), 1, 60, radioCP, gwCP)
	runUntilConfirmed(t, s, 2*time.Hour, radioCP, gwCP)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, radioCP, gwCP)
}

// The channel stays busy for 2 minutes: the radio gives up what it
// queued (DROP busy); the checkpoint retries; still exactly once.
func TestRadioBusyChannel(t *testing.T) {
	s := newSim(t, 13, gwfake.Profile{Loss: 0.05, MaxDelay: time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	rng := rand.New(rand.NewPCG(13, 13))
	massStart(t, s, rng, 1, 20, cp)
	cp.UV.SetBusy(true)
	massStart(t, s, rng, 21, 20, cp)
	s.Run(2 * time.Minute)
	cp.UV.SetBusy(false)
	runUntilConfirmed(t, s, 2*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// The radio is unplugged for 10 minutes mid-race: entries are kept and
// sent after it's plugged back in.
func TestRadioUnplugged(t *testing.T) {
	s := newSim(t, 14, gwfake.Profile{Loss: 0.1, MaxDelay: time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	rng := rand.New(rand.NewPCG(14, 14))
	massStart(t, s, rng, 1, 20, cp)
	s.UnplugRadio(cp)
	massStart(t, s, rng, 21, 30, cp)
	s.Run(10 * time.Minute)
	if allConfirmed(t, cp) {
		t.Fatal("confirmed while the radio was unplugged")
	}
	if err := s.PlugRadio(cp); err != nil {
		t.Fatal(err)
	}
	runUntilConfirmed(t, s, 2*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// A heavy-loss channel forces HQ's gap requests to the radio checkpoint
// and the radio's acks to compete with its reports.
func TestRadioHeavyLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	s := newSim(t, 15, gwfake.Profile{Loss: 0.35, Dup: 0.2, MaxDelay: 4 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	massStart(t, s, rand.New(rand.NewPCG(15, 15)), 1, 150, cp)
	took := runUntilConfirmed(t, s, 4*time.Hour, cp)
	t.Logf("radio checkpoint at 35%% loss: 150 confirmed %v after the last; %d frames on air", took, s.Radio.Sent())
	s.Run(15 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// A radio checkpoint restarted mid-race (kill -9) recovers from its
// database, with the radio still plugged in.
func TestRadioCheckpointRestart(t *testing.T) {
	s := newSim(t, 16, gwfake.Profile{Loss: 0.1, MaxDelay: time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addRadioNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	rng := rand.New(rand.NewPCG(16, 16))
	massStart(t, s, rng, 1, 30, cp)
	cp.RB.Detach(nil)
	if err := s.PlugRadio(cp); err != nil { // the app restarts and reconnects
		t.Fatal(err)
	}
	if err := s.Restart(cp); err != nil {
		t.Fatal(err)
	}
	massStart(t, s, rng, 31, 20, cp)
	runUntilConfirmed(t, s, 2*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// A checkpoint swaps radios mid-race, with reports in flight: graywolf
// to a UV-K5 and back (the Station page's radio type; the app restarts
// over the same database). Every passage still arrives exactly once.
func TestRadioSwapMidRace(t *testing.T) {
	s := newSim(t, 17, gwfake.Profile{Loss: 0.15, Dup: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "W1RAD-7", CheckpointSettings("AS1", hqCall))
	rng := rand.New(rand.NewPCG(17, 17))
	massStart(t, s, rng, 1, 30, cp)
	if err := s.SwitchRadio(cp, radiokind.UVK5); err != nil {
		t.Fatal(err)
	}
	massStart(t, s, rng, 31, 30, cp)
	s.Run(2 * time.Minute)
	if sent, _ := cp.Store.RadioChanges(ctx, 0, 0); len(sent) == 0 {
		t.Fatal("nothing went out through the UV-K5")
	}
	if err := s.SwitchRadio(cp, radiokind.Graywolf); err != nil {
		t.Fatal(err)
	}
	massStart(t, s, rng, 61, 20, cp)
	took := runUntilConfirmed(t, s, 2*time.Hour, cp)
	t.Logf("two radio swaps: 80 runners confirmed %v after the last", took)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// HQ swaps radios mid-race too: checkpoints keep reporting to the same
// callsign, now heard by the UV-K5. (A real callsign: a UV-K5 sends
// nothing as N0CALL.)
func TestHQRadioSwap(t *testing.T) {
	const radioHQ = "W1HQ-10"
	s := newSim(t, 18, gwfake.Profile{Loss: 0.1, MaxDelay: time.Second})
	hqNode := addNode(t, s, radioHQ, HQSettings())
	cp := addNode(t, s, "N0CALL-8", CheckpointSettings("AS2", radioHQ))
	rng := rand.New(rand.NewPCG(18, 18))
	massStart(t, s, rng, 1, 30, cp)
	if err := s.SwitchRadio(hqNode, radiokind.UVK5); err != nil {
		t.Fatal(err)
	}
	massStart(t, s, rng, 31, 30, cp)
	runUntilConfirmed(t, s, 2*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}
