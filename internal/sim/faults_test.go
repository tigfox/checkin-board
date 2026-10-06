package sim

import (
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

// Fault injection (phase 12d) at whole-node level.

// graywolf's API is unreachable for a while at both ends (restarts,
// a wedged service): the keypad keeps logging, nothing is lost or
// doubled once it's back.
func TestGraywolfAPIOutageMidRace(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	s := newSim(t, 31, gwfake.Profile{Loss: 0.2, Dup: 0.1, MaxDelay: 3 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	for bib := store.Bib(1); bib <= 120; bib++ {
		switch bib {
		case 20:
			cp.GW.SetAPIDown(true)
		case 50:
			hqNode.GW.SetAPIDown(true)
		case 80:
			cp.GW.SetAPIDown(false)
		case 100:
			hqNode.GW.SetAPIDown(false)
		}
		logBib(t, s, cp, bib)
		s.Run(10 * time.Second)
	}
	runUntilConfirmed(t, s, 4*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// Both apps are killed and restarted repeatedly mid-race (kill -9, power
// cuts): engines lose all memory, databases and graywolf survive.
func TestRepeatedRestartsMidRace(t *testing.T) {
	if testing.Short() {
		t.Skip("simulation")
	}
	s := newSim(t, 32, gwfake.Profile{Loss: 0.25, Dup: 0.1, MaxDelay: 3 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	for bib := store.Bib(1); bib <= 150; bib++ {
		logBib(t, s, cp, bib)
		s.Run(7 * time.Second)
		if bib%17 == 0 {
			if err := s.Restart(cp); err != nil {
				t.Fatal(err)
			}
		}
		if bib%23 == 0 {
			if err := s.Restart(hqNode); err != nil {
				t.Fatal(err)
			}
		}
	}
	runUntilConfirmed(t, s, 4*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}
