package web

import (
	"context"
	"slices"
	"sync"
	"time"

	"checkin-board/internal/hostmon"
	"checkin-board/internal/radiocheck"
	"checkin-board/internal/store"
)

const (
	// cableTTL is how long a missing-cable reading is reused: the panel
	// asks often, and each reading is two graywolf calls.
	cableTTL = 30 * time.Second
	// cableRetry is the wait after a reading that couldn't tell
	// (graywolf unreachable); the previous reading stands meanwhile.
	cableRetry = 5 * time.Second
	// cableTimeout bounds those calls.
	cableTimeout = 3 * time.Second
	// cableWarning is the panel's line (ASCII: the panel font).
	cableWarning = "Radio cable missing"
)

// cableWatch caches radiocheck.MissingCards. One caller refreshes at a
// time, without holding the lock over graywolf's calls; others get the
// current reading straight away.
type cableWatch struct {
	mu         sync.Mutex
	next       time.Time // when to refresh
	channel    int       // the GWChannel setting the reading is for
	refreshing bool
	missing    []string
}

// hostSnapshot is the node's health. The sound cards are left out when
// graywolf runs on another machine: they aren't graywolf's.
func (s *server) hostSnapshot() hostmon.Snapshot {
	var h hostmon.Snapshot
	if s.Host != nil {
		h = s.Host.Snapshot()
	}
	if s.GraywolfRemote {
		h.SoundCards, h.SoundCardsKnown = nil, false
	}
	return h
}

// missingCards names the radio channel's sound cards that aren't on this
// node (cached), and logs when that changes.
func (s *server) missingCards(cfg store.Settings) []string {
	c := &s.cable
	c.mu.Lock()
	now := s.now()
	stale := c.next.IsZero() || !now.Before(c.next) || c.channel != cfg.GWChannel
	if !stale || c.refreshing {
		m := c.missing
		c.mu.Unlock()
		return m
	}
	c.refreshing = true
	c.mu.Unlock()

	// Not the request's context: a client hanging up mustn't decide the
	// reading everyone else sees.
	ctx, cancel := context.WithTimeout(context.Background(), cableTimeout)
	missing, ok := radiocheck.MissingCards(ctx, s.Graywolf, cfg, s.hostSnapshot())
	cancel()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshing = false
	if !ok {
		c.next = now.Add(cableRetry) // keep the previous reading
		return c.missing
	}
	switch {
	case len(missing) > 0 && !slices.Equal(missing, c.missing):
		s.log.Warn("radio cable missing: graywolf's sound card is not on this node", "cards", missing)
	case len(missing) == 0 && len(c.missing) > 0:
		s.log.Info("radio cable back", "cards", c.missing)
	}
	c.next, c.channel, c.missing = now.Add(cableTTL), cfg.GWChannel, missing
	return missing
}
