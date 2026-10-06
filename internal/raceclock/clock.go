// Package raceclock is the race clock: a browser-synced, monotonic
// time source for nodes with no RTC, GPS or internet. Ported from
// graywolf pkg/race (our own code). Design: spec section 6.
package raceclock

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Source says where the race clock's time currently comes from.
type Source string

const (
	// SourceUnsynced: no browser sync since startup and the OS
	// clock isn't disciplined. Times are recorded but flagged.
	SourceUnsynced Source = "unsynced"
	// SourceSystem: the OS clock is disciplined (NTP/chrony).
	SourceSystem Source = "system"
	// SourceBrowser: set from a volunteer's browser via Sync.
	SourceBrowser Source = "browser"
)

// MaxSyncRTT rejects syncs whose round trip was too slow to trust.
const MaxSyncRTT = 10 * time.Second

// ErrInvalidSync is returned by Sync for implausible input.
var ErrInvalidSync = errors.New("raceclock: invalid clock sync")

var (
	minPlausible = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	maxPlausible = time.Date(2100, 12, 31, 0, 0, 0, 0, time.UTC)
)

// Clock is the race clock. Nodes have no RTC, GPS, or internet, and
// checkin-board runs unprivileged, so instead of setting the OS clock the
// race clock remembers a browser-supplied wall time plus the monotonic
// instant it arrived, and counts forward from there. OS clock steps
// after the sync therefore don't move race times.
//
// The sync is deliberately in-memory only: after a restart the
// relationship between the OS clock and true time is unknown again.
type Clock struct {
	now          func() time.Time
	systemSynced func() bool

	mu       sync.Mutex
	synced   bool
	syncWall time.Time // true wall time at the sync instant
	syncMono time.Time // now() at the sync instant; keeps its monotonic reading
}

// NewClock builds a race clock. now is normally time.Now and
// systemSynced reports whether the OS clock is disciplined (e.g. NTP,
// see spec section 6); nil selects time.Now and "never synced".
func NewClock(now func() time.Time, systemSynced func() bool) *Clock {
	if now == nil {
		now = time.Now
	}
	if systemSynced == nil {
		systemSynced = func() bool { return false }
	}
	return &Clock{now: now, systemSynced: systemSynced}
}

// Sync sets the race clock from a client's wall time. rtt is the
// client's measured round trip; half of it is added because the
// client's reading is that old when it arrives.
func (c *Clock) Sync(clientTime time.Time, rtt time.Duration) error {
	if clientTime.Before(minPlausible) || clientTime.After(maxPlausible) {
		return fmt.Errorf("%w: client time %v is implausible", ErrInvalidSync, clientTime)
	}
	if rtt < 0 || rtt > MaxSyncRTT {
		return fmt.Errorf("%w: round trip %v outside 0..%v", ErrInvalidSync, rtt, MaxSyncRTT)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Sample now() under the lock so concurrent Syncs and Status calls
	// are ordered and race time never steps backward. Never Round/UTC/In
	// syncMono: that strips the monotonic reading.
	c.synced = true
	c.syncWall = clientTime.UTC().Add(rtt / 2)
	c.syncMono = c.now()
	return nil
}

// Now returns the current race time and whether it is trustworthy.
// Unsynced, it returns the OS time so logging never blocks.
func (c *Clock) Now() (time.Time, bool) {
	st := c.Status()
	return st.Now, st.Source != SourceUnsynced
}

// Status is a snapshot for the UI and heartbeats.
type Status struct {
	Source Source
	Now    time.Time
	// SyncAge is the time since the last browser sync; zero unless
	// Source is SourceBrowser.
	SyncAge time.Duration
}

// Status reports the race clock's current time and source. A
// disciplined OS clock wins over a browser sync: NTP is more precise
// than a browser round trip.
func (c *Clock) Status() Status {
	if c.systemSynced() {
		return Status{Source: SourceSystem, Now: c.now()}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now() // after the lock, so a concurrent Sync can't make age negative
	if !c.synced {
		return Status{Source: SourceUnsynced, Now: now}
	}
	age := now.Sub(c.syncMono)
	return Status{Source: SourceBrowser, Now: c.syncWall.Add(age), SyncAge: age}
}
