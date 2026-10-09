package sim

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

const hqCall = "N0CALL-10"

func newSim(t *testing.T, seed uint64, prof gwfake.Profile) *Sim {
	t.Helper()
	s := New(seed, prof)
	t.Cleanup(s.Close)
	return s
}

func addNode(t *testing.T, s *Sim, call string, cfg store.Settings) *Node {
	t.Helper()
	n, err := s.AddNode(call, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func logBib(t *testing.T, s *Sim, n *Node, bib store.Bib) *store.LocalEntry {
	t.Helper()
	e, err := s.LogBib(n, bib)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func runUntilConfirmed(t *testing.T, s *Sim, limit time.Duration, cps ...*Node) time.Duration {
	t.Helper()
	d, err := s.RunUntilConfirmed(limit, cps...)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func assertExactlyOnce(t *testing.T, hqNode *Node, cps ...*Node) {
	t.Helper()
	diffs, err := CheckExactlyOnce(hqNode, cps...)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diffs {
		t.Error(d)
	}
}

func allConfirmed(t *testing.T, n *Node) bool {
	t.Helper()
	ok, err := AllConfirmed(n)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A mass start: 300 runners through one aid station in ~15 minutes over
// a lossy, duplicating, reordering channel.
func TestLossyChannelExactlyOnce(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	s := newSim(t, 1, gwfake.Profile{Loss: 0.3, Dup: 0.2, MaxDelay: 4 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))

	rng := rand.New(rand.NewPCG(7, 7))
	for bib := store.Bib(1); bib <= 300; bib++ {
		logBib(t, s, cp, bib)
		s.Run(time.Duration(1+rng.IntN(5)) * time.Second)
	}
	took := runUntilConfirmed(t, s, 4*time.Hour, cp)
	t.Logf("all 300 confirmed %v after the last bib; %d frames on air", took, s.Radio.Sent())
	// Latency guard (spec 9b): with fast retransmit and window 4, a mass
	// start at 30% loss drains in minutes.
	if took > 10*time.Minute {
		t.Errorf("drain took %v after the last bib, want under 10m at 30%% loss", took)
	}
	s.Run(10 * time.Minute) // let HQ see the final heartbeat
	assertExactlyOnce(t, hqNode, cp)
}

// Voids must cancel exactly the voided entries at HQ, including voids
// that overtake their originals.
func TestVoids(t *testing.T) {
	s := newSim(t, 2, gwfake.Profile{Loss: 0.25, Dup: 0.1, MaxDelay: 6 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))

	var entries []*store.LocalEntry
	for bib := store.Bib(1); bib <= 40; bib++ {
		entries = append(entries, logBib(t, s, cp, bib))
		s.Run(3 * time.Second)
	}
	// Double-tap bib 7 in a later second, then void the accidental copy:
	// bib 7's original must survive exactly once.
	dup := logBib(t, s, cp, 7)
	s.Run(30 * time.Second)
	for _, id := range []uint{entries[3].ID, entries[9].ID, entries[20].ID, entries[39].ID, dup.ID} {
		if _, err := cp.Store.VoidLocal(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	runUntilConfirmed(t, s, 3*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// Out-and-back: the same runners pass the same aid station twice.
func TestOutAndBack(t *testing.T) {
	s := newSim(t, 3, gwfake.Profile{Loss: 0.2, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("TURN", hqCall))
	for range 2 {
		for bib := store.Bib(1); bib <= 30; bib++ {
			logBib(t, s, cp, bib)
			s.Run(2 * time.Second)
		}
		s.Run(40 * time.Minute)
	}
	runUntilConfirmed(t, s, 2*time.Hour, cp)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
	if got, _ := hqNode.Store.EffectiveEntries(ctx, store.EntryFilter{CPCode: "TURN"}); len(got) != 60 {
		t.Fatalf("TURN passages = %d, want 60 (two per runner)", len(got))
	}
}

// A 40-minute total outage mid-race: nothing may be lost, and delivery
// resumes on its own.
func TestOutageRecovery(t *testing.T) {
	s := newSim(t, 4, gwfake.Profile{Loss: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS2", hqCall))
	for bib := store.Bib(1); bib <= 20; bib++ {
		logBib(t, s, cp, bib)
		s.Run(5 * time.Second)
	}
	s.Radio.SetDown(true)
	for bib := store.Bib(21); bib <= 80; bib++ {
		logBib(t, s, cp, bib)
		s.Run(40 * time.Second)
	}
	if allConfirmed(t, cp) {
		t.Fatal("entries confirmed during a total outage")
	}
	s.Radio.SetDown(false)
	took := runUntilConfirmed(t, s, time.Hour, cp)
	t.Logf("recovered %v after the outage ended", took)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// Three aid stations sharing one channel with HQ.
func TestThreeCheckpoints(t *testing.T) {
	if testing.Short() {
		t.Skip("long simulation")
	}
	s := newSim(t, 5, gwfake.Profile{Loss: 0.25, Dup: 0.15, MaxDelay: 3 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cps := []*Node{
		addNode(t, s, "N0CALL-11", CheckpointSettings("AS1", hqCall)),
		addNode(t, s, "N0CALL-12", CheckpointSettings("AS2", hqCall)),
		addNode(t, s, "N0CALL-13", CheckpointSettings("AS3", hqCall)),
	}
	for bib := store.Bib(1); bib <= 100; bib++ {
		for _, cp := range cps {
			logBib(t, s, cp, bib)
		}
		s.Run(4 * time.Second)
	}
	runUntilConfirmed(t, s, 4*time.Hour, cps...)
	s.Run(10 * time.Minute)
	assertExactlyOnce(t, hqNode, cps...)
}

// HQ loses a batch the checkpoint already saw ACKed (its database was
// restored from an old backup). Only the checkpoint's next heartbeat
// reveals the hole; HQ's gap request must recover it, and the batch,
// already acked in graywolf, must go out as a new message (spec 3.1).
func TestHeartbeatRevealsLostBatch(t *testing.T) {
	s := newSim(t, 6, gwfake.Profile{})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	logBib(t, s, cp, 1)
	s.Run(time.Minute)
	if !allConfirmed(t, cp) {
		t.Fatal("clean channel did not confirm")
	}
	if err := s.ReplaceStore(hqNode); err != nil {
		t.Fatal(err)
	}
	s.Run(20 * time.Minute) // heartbeat (5 min) + grace (90 s) + resend
	assertExactlyOnce(t, hqNode, cp)
}

// HQ's database is wiped but its graywolf still holds the day's
// reports: re-reading graywolf's inbox (the admin re-read action)
// restores everything without RF.
func TestWipedHQRecoversFromGraywolfInbox(t *testing.T) {
	s := newSim(t, 9, gwfake.Profile{Loss: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	for bib := store.Bib(1); bib <= 30; bib++ {
		logBib(t, s, cp, bib)
		s.Run(3 * time.Second)
	}
	runUntilConfirmed(t, s, time.Hour, cp)
	if err := s.ReplaceStore(hqNode); err != nil {
		t.Fatal(err)
	}
	if err := hqNode.Store.SetInboxSince(ctx, Start); err != nil {
		t.Fatal(err)
	}
	s.Radio.SetDown(true) // no RF needed
	s.Run(time.Minute)
	assertExactlyOnce(t, hqNode, cp)
}

// A spoofer must not turn the checkpoint's fast paths into an airtime
// storm on a dead link. APRS is unauthenticated, so the spoofer forges
// HQ's own call, and uses a fresh msgid on every frame so graywolf's
// (from, msgid, text) dedup doesn't swallow the flood.
func TestSpoofedTrafficBoundsAirtime(t *testing.T) {
	cases := []struct {
		name  string
		frame func(n int, firstBatch uint64) gwfake.Frame
		limit int // frames per hour from the checkpoint
	}{
		// ACKs that confirm nothing never expedite: only the normal ladder
		// for the window (4 batches) plus heartbeats.
		{"acks for unknown msgids", func(n int, _ uint64) gwfake.Frame {
			return gwfake.Frame{From: hqCall, To: "N0CALL-7", MsgID: fmt.Sprintf("9%04d", n), IsAck: true}
		}, 80},
		// A replayed ACK for a batch that was really acked confirms
		// nothing new, so it can't expedite either.
		{"replayed acks for an acked batch", func(_ int, first uint64) gwfake.Frame {
			return gwfake.Frame{From: hqCall, To: "N0CALL-7", MsgID: strconv.FormatUint(first, 10), IsAck: true}
		}, 80},
		// Gap requests requeue a batch at most once per requeueMinAge, and
		// only max_in_flight go on air at a time.
		{"gap requests", func(n int, _ uint64) gwfake.Frame {
			return gwfake.Frame{From: hqCall, To: "N0CALL-7", Text: "RC1 G AS1 1-20", MsgID: fmt.Sprintf("7%04d", n)}
		}, 4*60 + 20},
		// Gap requests from any other call are ignored outright.
		{"gap requests from a stranger", func(n int, _ uint64) gwfake.Frame {
			return gwfake.Frame{From: "N0CALL-14", To: "N0CALL-7", Text: "RC1 G AS1 1-20", MsgID: fmt.Sprintf("8%04d", n)}
		}, 80},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := newSim(t, 8, gwfake.Profile{})
			_ = addNode(t, s, hqCall, HQSettings())
			cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
			for bib := store.Bib(1); bib <= 4; bib++ { // one batch, delivered and acked
				logBib(t, s, cp, bib)
			}
			s.Run(30 * time.Second)
			if !allConfirmed(t, cp) {
				t.Fatal("first batch not confirmed on a clean channel")
			}
			firstBatch := cp.GW.TransmissionsWithPrefix("RC1 R ")[0].ID
			for bib := store.Bib(5); bib <= 40; bib++ { // several batches outstanding
				logBib(t, s, cp, bib)
			}
			// Nothing the checkpoint sends gets through from here on; it
			// only hears the spoofer.
			s.Radio.SetDown(true)
			s.Run(5 * time.Minute)
			before := len(cp.GW.Transmissions())
			n := 0
			for end := s.Clock.Now().Add(time.Hour); s.Clock.Now().Before(end); n++ {
				s.Radio.SetDown(false)
				s.Radio.Inject(c.frame(n, firstBatch))
				s.Radio.Deliver()
				s.Radio.SetDown(true)
				s.Run(5 * time.Second)
			}
			sent := len(cp.GW.Transmissions()) - before
			t.Logf("%d frames in an hour of %d spoofed %s (limit %d)", sent, n, c.name, c.limit)
			if sent > c.limit {
				t.Fatalf("spoofed %s drove %d frames/hour, want <= %d", c.name, sent, c.limit)
			}
		})
	}
}

// The secured phase (spec 4.7): a checkpoint loses its link with
// entries still unconfirmed, is packed up (no transmissions on the way),
// carried back to HQ, and checks in: everything HQ lacks goes over at
// once, HQ ends exactly-once, and the node reaches checked_in by itself.
func TestSecureTravelAndFinalCheckIn(t *testing.T) {
	s := newSim(t, 10, gwfake.Profile{Loss: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS3", hqCall))
	for bib := store.Bib(1); bib <= 30; bib++ {
		logBib(t, s, cp, bib)
		s.Run(5 * time.Second)
	}
	s.Radio.SetDown(true) // the last runners' batches never get out
	for bib := store.Bib(31); bib <= 50; bib++ {
		logBib(t, s, cp, bib)
		s.Run(10 * time.Second)
	}
	if err := s.SetState(cp, store.RaceComplete); err != nil {
		t.Fatal(err)
	}
	s.Run(time.Minute)
	if err := s.SetState(cp, store.RaceSecured); err != nil {
		t.Fatal(err)
	}
	if allConfirmed(t, cp) {
		t.Fatal("setup: expected unconfirmed entries")
	}

	// Two hours of driving back: the node must stay off the air.
	before := len(cp.GW.Transmissions())
	s.Run(2 * time.Hour)
	if n := len(cp.GW.Transmissions()) - before; n != 0 {
		t.Fatalf("secured node transmitted %d frames on the way back", n)
	}

	// At HQ: radio back in range, final check-in.
	s.Radio.SetDown(false)
	if err := s.SetState(cp, store.RaceCheckingIn); err != nil {
		t.Fatal(err)
	}
	took := runUntilConfirmed(t, s, 30*time.Minute, cp)
	for i := 0; i < 120; i++ {
		if st, _ := s.State(cp); st == store.RaceCheckedIn {
			break
		}
		s.Step()
	}
	if st, _ := s.State(cp); st != store.RaceCheckedIn {
		t.Fatalf("state = %s, want checked_in", st)
	}
	t.Logf("final check-in delivered the backlog in %v", took)
	if took > 5*time.Minute {
		t.Errorf("check-in took %v at close range, want minutes at most", took)
	}
	assertExactlyOnce(t, hqNode, cp)
}

// A checkpoint early on the course closes while the race goes on (4.7):
// HQ sees it as closed, and HQ's own race is untouched.
func TestCheckpointClosesWhileRaceContinues(t *testing.T) {
	s := newSim(t, 41, gwfake.Profile{Loss: 0.1, MaxDelay: 2 * time.Second})
	hqNode := addNode(t, s, hqCall, HQSettings())
	cp := addNode(t, s, "N0CALL-7", CheckpointSettings("AS1", hqCall))
	for bib := store.Bib(1); bib <= 20; bib++ {
		logBib(t, s, cp, bib)
		s.Run(5 * time.Second)
	}
	if err := s.SetState(cp, store.RaceComplete); err != nil {
		t.Fatal(err)
	}
	runUntilConfirmed(t, s, time.Hour, cp)
	s.Run(10 * time.Minute) // at least one heartbeat after closing
	sts, err := hqNode.Store.ListStatuses(ctx)
	if err != nil || len(sts) != 1 || sts[0].ClosedAt == nil {
		t.Fatalf("HQ status = %+v, %v", sts, err)
	}
	if state, _ := s.State(hqNode); state != store.RaceActive {
		t.Fatalf("HQ state = %s, want the race still active", state)
	}
	assertExactlyOnce(t, hqNode, cp)
}
