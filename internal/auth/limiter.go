package auth

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Brute-force limiting: maxFailures wrong passwords per key within
// window, then a lockout that doubles each time (up to maxLockout).
const (
	maxFailures  = 5
	window       = time.Minute
	firstLockout = time.Minute
	maxLockout   = 15 * time.Minute
	maxKeys      = 10_000 // bounds memory under a spray from many addresses
)

// ErrRateLimited is returned while a key is locked out.
var ErrRateLimited = errors.New("auth: too many attempts")

// RateLimitError says how long to wait.
type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("auth: too many attempts; try again in %v", e.RetryAfter.Round(time.Second))
}

func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type limiterEntry struct {
	failures    []time.Time
	lockedUntil time.Time
	lockout     time.Duration
}

// Limiter counts failed logins per key (client address + role).
type Limiter struct {
	now func() time.Time

	mu   sync.Mutex
	keys map[string]*limiterEntry
}

// NewLimiter returns an empty limiter.
func NewLimiter(now func() time.Time) *Limiter {
	return &Limiter{now: now, keys: map[string]*limiterEntry{}}
}

// Attempt records a login attempt for key and reports whether it may
// proceed. Every attempt counts against the budget until a success
// clears it (Succeed), so parallel guesses can't outrun the count.
// maxFailures attempts within window lock the key, doubling each time.
func (l *Limiter) Attempt(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.keys[key]
	if e == nil {
		if len(l.keys) >= maxKeys {
			l.evictLocked(now)
		}
		if len(l.keys) >= maxKeys {
			// Still full of active entries: under a spray, refuse new
			// clients rather than forget existing lockouts.
			return &RateLimitError{RetryAfter: window}
		}
		e = &limiterEntry{}
		l.keys[key] = e
	}
	if now.Before(e.lockedUntil) {
		return &RateLimitError{RetryAfter: e.lockedUntil.Sub(now)}
	}
	recent := e.failures[:0]
	for _, t := range e.failures {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	e.failures = append(recent, now)
	if len(e.failures) > maxFailures {
		e.lockout = min(max(firstLockout, 2*e.lockout), maxLockout)
		e.lockedUntil = now.Add(e.lockout)
		e.failures = nil
		return &RateLimitError{RetryAfter: e.lockout}
	}
	return nil
}

// Succeed clears key after a successful login.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// evictLocked drops entries that are neither locked nor recently
// attempting.
func (l *Limiter) evictLocked(now time.Time) {
	for k, e := range l.keys {
		if !now.Before(e.lockedUntil) && (len(e.failures) == 0 || now.Sub(e.failures[len(e.failures)-1]) >= window) {
			delete(l.keys, k)
		}
	}
}
