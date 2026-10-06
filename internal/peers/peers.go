// Package peers manages graywolf's per-conversation retry setting for
// race peers (spec 3.3). The app owns retries for race traffic, so it
// turns graywolf's own retry ladder off (wait_for_ack=false) for each
// peer, after backing up the operator's original setting, and puts the
// original back when the race is complete.
package peers

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
)

// Prefs is the subset of the graywolf client this package uses.
type Prefs interface {
	ConversationPrefs(ctx context.Context, kind, key string) (graywolf.ConversationPrefs, error)
	SetConversationPrefs(ctx context.Context, kind, key string, p graywolf.ConversationPrefs) (graywolf.ConversationPrefs, error)
}

// Ensure turns graywolf's retries off for every call in calls. Each
// peer's original prefs are backed up first; an existing backup is kept,
// so running Ensure again after a restart never records the app's own
// setting as the original. Blank calls are skipped. It stops at the
// first error; calling it again resumes safely.
func Ensure(ctx context.Context, gw Prefs, st *store.Store, calls []string) error {
	for _, raw := range calls {
		call := strings.ToUpper(strings.TrimSpace(raw))
		if call == "" {
			continue
		}
		if !store.ValidStationCall(call) {
			return fmt.Errorf("peers: %q is not a station callsign", raw)
		}
		cur, err := gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, call)
		if err != nil {
			return fmt.Errorf("peers: read %s prefs: %w", call, err)
		}
		if _, err := st.SavePeerPrefs(ctx, store.PeerPrefs{Callsign: call, SendPath: cur.SendPath, WaitForAck: cur.WaitForAck}); err != nil {
			return fmt.Errorf("peers: back up %s prefs: %w", call, err)
		}
		if !cur.WaitForAck {
			continue
		}
		off := graywolf.ConversationPrefs{SendPath: cur.SendPath, WaitForAck: false}
		if _, err := gw.SetConversationPrefs(ctx, graywolf.ThreadKindDM, call, off); err != nil {
			return fmt.Errorf("peers: turn off %s retries: %w", call, err)
		}
	}
	return nil
}

// Restore writes every backed-up original back to graywolf and deletes
// each backup once its restore succeeded. Failures are joined; their
// backups stay for a later retry.
func Restore(ctx context.Context, gw Prefs, st *store.Store) error {
	backups, err := st.ListPeerPrefs(ctx)
	if err != nil {
		return fmt.Errorf("peers: list backups: %w", err)
	}
	var errs []error
	for _, b := range backups {
		orig := graywolf.ConversationPrefs{SendPath: b.SendPath, WaitForAck: b.WaitForAck}
		if _, err := gw.SetConversationPrefs(ctx, graywolf.ThreadKindDM, b.Callsign, orig); err != nil {
			errs = append(errs, fmt.Errorf("peers: restore %s: %w", b.Callsign, err))
			continue
		}
		if err := st.DeletePeerPrefs(ctx, b.Callsign); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// retryAfter is how long a failed Ensure for a call is not retried.
const retryAfter = 5 * time.Minute

// Ensurer caches Ensure per callsign so engines can call it before
// every send to a peer at no cost after the first success. Safe for
// concurrent use.
type Ensurer struct {
	gw  Prefs
	st  *store.Store
	now func() time.Time

	mu      sync.Mutex
	done    map[string]bool
	failed  map[string]time.Time
	lastErr map[string]error
}

// NewEnsurer returns an Ensurer. now nil means time.Now.
func NewEnsurer(gw Prefs, st *store.Store, now func() time.Time) *Ensurer {
	if now == nil {
		now = time.Now
	}
	return &Ensurer{gw: gw, st: st, now: now, done: map[string]bool{}, failed: map[string]time.Time{}, lastErr: map[string]error{}}
}

// Ensure turns graywolf's retries off for call once. After a failure it
// returns the same error without retrying for retryAfter.
func (e *Ensurer) Ensure(ctx context.Context, call string) error {
	call = strings.ToUpper(strings.TrimSpace(call))
	e.mu.Lock()
	if e.done[call] {
		e.mu.Unlock()
		return nil
	}
	if at, ok := e.failed[call]; ok && e.now().Sub(at) < retryAfter {
		err := e.lastErr[call]
		e.mu.Unlock()
		return err
	}
	e.mu.Unlock()

	err := Ensure(ctx, e.gw, e.st, []string{call})
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		e.failed[call], e.lastErr[call] = e.now(), err
		return err
	}
	e.done[call] = true
	delete(e.failed, call)
	delete(e.lastErr, call)
	return nil
}

// Reset forgets every cached result, e.g. after Restore at race end, so
// the next race's sends ensure their peers again.
func (e *Ensurer) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	clear(e.done)
	clear(e.failed)
	clear(e.lastErr)
}
