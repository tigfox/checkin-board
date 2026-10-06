package linkcheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// Run limits (spec 4.8.5).
const (
	DefaultCount   = 5
	MaxCount       = 10 // with ACKs and the reply, about 12 frames of airtime per side
	DefaultSpacing = 10 * time.Second
	MinInterval    = 2 * time.Minute
	// HQCode is the station code HQ puts in its probes.
	HQCode = "HQ"
)

var (
	// ErrNotAllowed: this node can't run a link check now (no role, or
	// the race is past the point where airtime should be spent on it).
	ErrNotAllowed = errors.New("linkcheck: not allowed now")
	// ErrNeedsConfirm: the race is active; the operator must confirm.
	ErrNeedsConfirm = errors.New("linkcheck: the race is active; confirm to run a link check anyway")
	// ErrBusy: a link check is already requested or running.
	ErrBusy = errors.New("linkcheck: a link check is already running")
	// ErrInvalid: a bad target or probe count.
	ErrInvalid = errors.New("linkcheck: invalid request")
)

// TooSoonError: runs must be MinInterval apart.
type TooSoonError struct{ RetryAfter time.Duration }

func (e *TooSoonError) Error() string {
	return fmt.Sprintf("linkcheck: link checks are at least %s apart; try again in %s", MinInterval, e.RetryAfter.Round(time.Second))
}

// Req asks for a run. To is the peer's callsign (HQ only: a checkpoint
// always probes its HQ call). Count 0 means DefaultCount.
type Req struct {
	To      string
	Count   int
	Confirm bool
	Source  string // "admin" or "cli"
}

// Request validates req against the node's settings and records it for
// the service to run. It needs only the store, so the CLI can call it
// with the service running in another process.
func Request(ctx context.Context, st *store.Store, req Req, now time.Time) (store.LinkCheck, error) {
	cfg, err := st.GetSettings(ctx)
	if err != nil {
		return store.LinkCheck{}, err
	}
	switch cfg.RaceState {
	case store.RaceSetup:
	case store.RaceActive:
		if !req.Confirm {
			return store.LinkCheck{}, ErrNeedsConfirm
		}
	default:
		return store.LinkCheck{}, fmt.Errorf("%w: the race is %s", ErrNotAllowed, cfg.RaceState)
	}
	c := store.LinkCheck{Count: req.Count, SpacingSec: int(DefaultSpacing / time.Second), Source: req.Source, RequestedAt: now}
	if c.Count == 0 {
		c.Count = DefaultCount
	}
	if c.Count < 1 || c.Count > MaxCount {
		return store.LinkCheck{}, fmt.Errorf("%w: probe count %d outside 1..%d", ErrInvalid, c.Count, MaxCount)
	}
	switch cfg.Role {
	case store.RoleCheckpoint:
		c.PeerCall, c.StationCode = strings.ToUpper(cfg.HQCall), cfg.CheckpointCode
		if !store.ValidStationCall(c.PeerCall) || !wire.ValidCheckpointCode(c.StationCode) {
			return store.LinkCheck{}, fmt.Errorf("%w: set the HQ callsign and checkpoint code first", ErrNotAllowed)
		}
	case store.RoleHQ:
		c.PeerCall, c.StationCode = strings.ToUpper(strings.TrimSpace(req.To)), HQCode
		if !store.ValidStationCall(c.PeerCall) {
			return store.LinkCheck{}, fmt.Errorf("%w: give the checkpoint's callsign to probe", ErrInvalid)
		}
	default:
		return store.LinkCheck{}, fmt.Errorf("%w: set this node's role first", ErrNotAllowed)
	}
	if last, err := st.ListLinkChecks(ctx, 1); err != nil {
		return store.LinkCheck{}, err
	} else if len(last) == 1 {
		if st := last[0].State; st == store.LinkCheckRequested || st == store.LinkCheckRunning {
			return store.LinkCheck{}, ErrBusy
		}
		// A run cancelled before it started never transmitted; any other
		// counts, so cancelling can't be used to probe back to back.
		if wait := last[0].RequestedAt.Add(MinInterval).Sub(now); wait > 0 && !(last[0].State == store.LinkCheckCancelled && last[0].StartedAt == nil) {
			return store.LinkCheck{}, &TooSoonError{RetryAfter: wait}
		}
	}
	out, err := st.CreateLinkCheck(ctx, c)
	if errors.Is(err, store.ErrConflict) {
		return store.LinkCheck{}, ErrBusy
	}
	return out, err
}
