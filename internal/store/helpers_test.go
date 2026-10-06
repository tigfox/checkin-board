package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// newTestStore opens a fresh in-memory store running the real
// migrations, so tests exercise the shipped schema.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenMemory()
	if err != nil {
		t.Fatalf("OpenMemory: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var ctx = context.Background()

// t0 is a fixed race-morning instant used across store tests.
var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }

// fakeTime is a controllable clock for row-creation timestamps.
type fakeTime struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeTime) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}
