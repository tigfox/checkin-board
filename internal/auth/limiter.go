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

// Allow reports whether key may try now.
func (l *Limiter) Allow(key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.keys[key]
	if e == nil {
		return nil
	}
	if now := l.now(); now.Before(e.lockedUntil) {
		return &RateLimitError{RetryAfter: e.lockedUntil.Sub(now)}
	}
	return nil
}

// Fail records a failed attempt, locking key out after maxFailures in
// window.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.keys[key]
	if e == nil {
		if len(l.keys) >= maxKeys {
			l.evictLocked(now)
		}
		e = &limiterEntry{}
		l.keys[key] = e
	}
	recent := e.failures[:0]
	for _, t := range e.failures {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	e.failures = append(recent, now)
	if len(e.failures) >= maxFailures {
		e.lockout = min(max(firstLockout, 2*e.lockout), maxLockout)
		e.lockedUntil = now.Add(e.lockout)
		e.failures = nil
	}
}

// Succeed clears key after a successful login.
func (l *Limiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.keys, key)
}

// evictLocked drops entries that are neither locked nor recently
// failing; if that frees nothing, it drops everything unlocked.
func (l *Limiter) evictLocked(now time.Time) {
	for k, e := range l.keys {
		if !now.Before(e.lockedUntil) && (len(e.failures) == 0 || now.Sub(e.failures[len(e.failures)-1]) >= window) {
			delete(l.keys, k)
		}
	}
	if len(l.keys) >= maxKeys {
		for k, e := range l.keys {
			if !now.Before(e.lockedUntil) {
				delete(l.keys, k)
			}
		}
	}
}
