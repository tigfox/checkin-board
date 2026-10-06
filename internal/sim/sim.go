// Package sim runs whole checkin-board nodes (store, fake graywolf,
// engines, inbox reader) on a simulated lossy RF channel in simulated
// time, so hours of race run in seconds (spec phase 7, section 9b).
// It is test support: nothing in the app imports it.
package sim

import (
	"context"
	"fmt"
	"sync"
	"time"

	"checkin-board/internal/checkpoint"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/hq"
	"checkin-board/internal/inbox"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

var ctx = context.Background()

// Start is the simulated race morning.
var Start = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

// Clock is the shared simulated clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// Now returns the simulated time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *Clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Node is one station: graywolf plus checkin-board.
type Node struct {
	Call     string
	Settings store.Settings
	Store    *store.Store
	GW       *gwfake.Station
	CP       *checkpoint.Engine
	HQ       *hq.Engine
	Inbox    *inbox.Reader
	clock    *raceclock.Clock
}

// Sim is a channel with nodes on it.
type Sim struct {
	Clock *Clock
	Radio *gwfake.Radio
	Nodes []*Node
}

// New returns a simulation on a channel with the given profile.
func New(seed uint64, prof gwfake.Profile) *Sim {
	c := &Clock{t: Start}
	return &Sim{Clock: c, Radio: gwfake.NewRadio(seed, prof, c.Now)}
}

// AddNode creates a node with its own database and graywolf on the channel.
func (s *Sim) AddNode(call string, settings store.Settings) (*Node, error) {
	gw := gwfake.New(call)
	gw.Now = s.Clock.Now
	s.Radio.Attach(gw)
	n := &Node{Call: call, Settings: settings, GW: gw}
	if err := s.build(n); err != nil {
		return nil, err
	}
	s.Nodes = append(s.Nodes, n)
	return n, nil
}

// build gives n a fresh store and engines (also used to simulate a
// node whose database was wiped or restored from an old backup).
func (s *Sim) build(n *Node) error {
	st, err := store.OpenMemory()
	if err != nil {
		return err
	}
	st.SetClock(s.Clock.Now)
	if _, err := st.SaveSettings(ctx, n.Settings); err != nil {
		return err
	}
	clock := raceclock.NewClock(s.Clock.Now, func() bool { return true })
	ens := peers.NewEnsurer(n.GW, st, s.Clock.Now)
	cp, err := checkpoint.New(checkpoint.Config{Store: st, Graywolf: n.GW, Clock: clock, Now: s.Clock.Now, Peers: ens})
	if err != nil {
		return err
	}
	hqe, err := hq.New(hq.Config{Store: st, Graywolf: n.GW, Clock: clock, Now: s.Clock.Now, Peers: ens})
	if err != nil {
		return err
	}
	var d inbox.Dispatcher = cp
	if n.Settings.Role == store.RoleHQ {
		d = hqe
	}
	reader, err := inbox.New(inbox.Config{Graywolf: n.GW, Store: st, Dispatcher: d})
	if err != nil {
		return err
	}
	if n.Store != nil {
		_ = n.Store.Close()
	}
	n.Store, n.CP, n.HQ, n.Inbox, n.clock = st, cp, hqe, reader, clock
	return nil
}

// ReplaceStore gives n an empty database, as after a wipe or a restore
// from an old backup. Its inbox reads only graywolf rows from now on
// (as after an admin Reset), so old reports aren't simply re-read.
func (s *Sim) ReplaceStore(n *Node) error {
	if err := s.build(n); err != nil {
		return err
	}
	return n.Store.EnsureInboxSince(ctx, s.Clock.Now().Add(time.Second))
}

// Close releases every node's database.
func (s *Sim) Close() {
	for _, n := range s.Nodes {
		_ = n.Store.Close()
	}
}

// Step advances one second: deliver due frames, let every node read its
// graywolf inbox, then tick every node's engine. Errors are normal on a
// bad channel and are ignored, as the real loops log and carry on.
func (s *Sim) Step() {
	s.Clock.advance(time.Second)
	s.Radio.Deliver()
	for _, n := range s.Nodes {
		_ = n.Inbox.CatchUp(ctx)
		// Re-read settings: the lifecycle (and the engine itself, on
		// finishing a check-in) changes the race state.
		cfg, err := n.Store.GetSettings(ctx)
		if err != nil {
			continue
		}
		switch cfg.Role {
		case store.RoleCheckpoint:
			_ = n.CP.Tick(ctx, cfg)
		case store.RoleHQ:
			if cfg.RaceState == store.RaceActive || cfg.RaceState == store.RaceComplete {
				_ = n.HQ.Tick(ctx, cfg)
			}
		}
	}
}

// Run steps for d of simulated time.
func (s *Sim) Run(d time.Duration) {
	for end := s.Clock.Now().Add(d); s.Clock.Now().Before(end); {
		s.Step()
	}
}

// SetState moves a node's race state (as the lifecycle actions do).
func (s *Sim) SetState(n *Node, state string) error {
	cfg, err := n.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	cfg.RaceState = state
	_, err = n.Store.SaveSettings(ctx, cfg)
	return err
}

// State returns a node's current race state.
func (s *Sim) State(n *Node) (string, error) {
	cfg, err := n.Store.GetSettings(ctx)
	return cfg.RaceState, err
}

// LogBib logs a bib on a checkpoint's keypad at the current time.
func (s *Sim) LogBib(n *Node, bib store.Bib) (*store.LocalEntry, error) {
	now, synced := n.clock.Now()
	return n.Store.LogLocal(ctx, n.Settings.CheckpointCode, bib, now, synced)
}

// AllConfirmed reports whether a checkpoint has nothing left to deliver.
func AllConfirmed(n *Node) (bool, error) {
	st, err := n.Store.OutboxStats(ctx)
	if err != nil {
		return false, err
	}
	return st.Unconfirmed == 0 && st.PendingBatches == 0, nil
}

// RunUntilConfirmed steps until every checkpoint is confirmed, returning
// how long that took, or an error after limit.
func (s *Sim) RunUntilConfirmed(limit time.Duration, cps ...*Node) (time.Duration, error) {
	start := s.Clock.Now()
	for s.Clock.Now().Sub(start) < limit {
		done := true
		for _, cp := range cps {
			ok, err := AllConfirmed(cp)
			if err != nil {
				return 0, err
			}
			done = done && ok
		}
		if done {
			return s.Clock.Now().Sub(start), nil
		}
		s.Step()
	}
	return 0, fmt.Errorf("checkpoints not fully confirmed after %v of simulated time", limit)
}

// Passage is one runner passage: checkpoint, bib, and the second.
type Passage struct {
	CP   string
	Bib  store.Bib
	Unix int64
}

// Expected counts every non-voided entry a checkpoint logged, per
// passage: a multiset, so two same-second entries must both arrive.
func Expected(n *Node) (map[Passage]int, error) {
	views, err := n.Store.ListLocal(ctx, 100000)
	if err != nil {
		return nil, err
	}
	out := map[Passage]int{}
	for _, v := range views {
		if !v.Voided {
			out[Passage{v.CPCode, v.Bib, v.TimeIn.Unix()}]++
		}
	}
	return out, nil
}

// CheckExactlyOnce compares HQ's effective entries with the union of the
// checkpoints' logs and describes every difference (nil if none):
// nothing missing, nothing extra, nothing doubled.
func CheckExactlyOnce(hqNode *Node, cps ...*Node) ([]string, error) {
	want := map[Passage]int{}
	for _, cp := range cps {
		exp, err := Expected(cp)
		if err != nil {
			return nil, err
		}
		for p, n := range exp {
			want[p] += n
		}
	}
	got, err := hqNode.Store.EffectiveEntries(ctx, store.EntryFilter{})
	if err != nil {
		return nil, err
	}
	var diffs []string
	if len(want) == 0 {
		diffs = append(diffs, "no checkpoint logged anything: the check would pass vacuously")
	}
	seen := map[Passage]bool{}
	for _, e := range got {
		p := Passage{e.CPCode, e.Bib, e.TimeIn.Unix()}
		switch {
		case want[p] == 0:
			diffs = append(diffs, fmt.Sprintf("HQ has an entry no checkpoint logged (or a voided one): %+v", e))
		case e.Count != want[p]:
			diffs = append(diffs, fmt.Sprintf("HQ counted %+v %d times, checkpoints logged it %d", p, e.Count, want[p]))
		}
		seen[p] = true
	}
	for p := range want {
		if !seen[p] {
			diffs = append(diffs, fmt.Sprintf("HQ is missing %+v", p))
		}
	}
	return diffs, nil
}

// CheckpointSettings returns settings for a checkpoint reporting to hqCall.
func CheckpointSettings(code, hqCall string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceState = store.RoleCheckpoint, code, hqCall, store.RaceActive
	return c
}

// HQSettings returns settings for the HQ node.
func HQSettings() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceState = store.RoleHQ, "START,FIN", store.RaceActive
	return c
}
