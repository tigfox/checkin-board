package raceclock

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTime is a controllable "time.Now". Advancing it models the passage
// of monotonic time; the wall reading is irrelevant to the race clock
// once synced, which is the whole point.
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

// A Pi with no RTC boots with fake-hwclock's last-shutdown time.
var bootWall = time.Date(2026, 9, 20, 18, 0, 0, 0, time.UTC)

func newTestClock(systemSynced bool) (*Clock, *fakeTime) {
	ft := &fakeTime{t: bootWall}
	c := NewClock(ft.Now, func() bool { return systemSynced })
	return c, ft
}

func TestClockUnsyncedFallsBackToSystem(t *testing.T) {
	c, _ := newTestClock(false)
	now, synced := c.Now()
	if synced {
		t.Fatal("fresh clock must report unsynced")
	}
	if !now.Equal(bootWall) {
		t.Fatalf("unsynced Now = %v, want system time %v", now, bootWall)
	}
	if st := c.Status(); st.Source != SourceUnsynced {
		t.Fatalf("Source = %q, want %q", st.Source, SourceUnsynced)
	}
}

func TestClockSystemSyncedNeedsNoBrowser(t *testing.T) {
	c, _ := newTestClock(true)
	now, synced := c.Now()
	if !synced || !now.Equal(bootWall) {
		t.Fatalf("Now = %v, %v; want system time, synced", now, synced)
	}
	if st := c.Status(); st.Source != SourceSystem {
		t.Fatalf("Source = %q, want %q", st.Source, SourceSystem)
	}
}

func TestClockBrowserSyncTracksElapsed(t *testing.T) {
	c, ft := newTestClock(false)
	browser := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	if err := c.Sync(browser, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	// Half the round trip is added: the browser's reading is ~100ms old
	// by the time the server sees it.
	now, synced := c.Now()
	if !synced || !now.Equal(browser.Add(100*time.Millisecond)) {
		t.Fatalf("Now = %v, %v; want %v, synced", now, synced, browser.Add(100*time.Millisecond))
	}
	ft.Advance(90 * time.Minute)
	now, _ = c.Now()
	if want := browser.Add(90*time.Minute + 100*time.Millisecond); !now.Equal(want) {
		t.Fatalf("after 90m Now = %v, want %v", now, want)
	}
	st := c.Status()
	if st.Source != SourceBrowser {
		t.Fatalf("Source = %q, want browser", st.Source)
	}
	if st.SyncAge != 90*time.Minute {
		t.Fatalf("SyncAge = %v, want 90m", st.SyncAge)
	}
}

func TestClockSystemSyncWinsOverBrowser(t *testing.T) {
	c, _ := newTestClock(true)
	if err := c.Sync(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), 0); err != nil {
		t.Fatal(err)
	}
	now, synced := c.Now()
	if !synced || !now.Equal(bootWall) {
		t.Fatalf("Now = %v; a disciplined OS clock should win over a browser sync", now)
	}
}

func TestClockResync(t *testing.T) {
	c, ft := newTestClock(false)
	first := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	if err := c.Sync(first, 0); err != nil {
		t.Fatal(err)
	}
	ft.Advance(time.Hour)
	second := time.Date(2026, 10, 5, 14, 0, 7, 0, time.UTC) // drifted 7s
	if err := c.Sync(second, 0); err != nil {
		t.Fatal(err)
	}
	if now, _ := c.Now(); !now.Equal(second) {
		t.Fatalf("Now = %v, want latest sync %v", now, second)
	}
	if st := c.Status(); st.SyncAge != 0 {
		t.Fatalf("SyncAge = %v, want 0 after resync", st.SyncAge)
	}
}

func TestClockSyncRejectsBadInput(t *testing.T) {
	good := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		client time.Time
		rtt    time.Duration
	}{
		{"zero time", time.Time{}, 0},
		{"before 2020", time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC), 0},
		{"far future", time.Date(2101, 1, 1, 0, 0, 0, 0, time.UTC), 0},
		{"negative rtt", good, -time.Millisecond},
		{"rtt too large", good, MaxSyncRTT + time.Millisecond},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newTestClock(false)
			if err := c.Sync(tc.client, tc.rtt); !errors.Is(err, ErrInvalidSync) {
				t.Fatalf("err = %v, want ErrInvalidSync", err)
			}
			if _, synced := c.Now(); synced {
				t.Fatal("rejected sync must not mark the clock synced")
			}
		})
	}
}

// With the real time.Now, the stored sync instant must keep its
// monotonic reading; otherwise an OS clock step (fake-hwclock, a late
// NTP sync, someone running `date`) would shift every race time.
func TestClockRealNowKeepsMonotonic(t *testing.T) {
	c := NewClock(time.Now, func() bool { return false })
	if err := c.Sync(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), 0); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	mono := c.syncMono
	c.mu.Unlock()
	if !strings.Contains(mono.String(), "m=") {
		t.Fatalf("sync instant lost its monotonic reading: %v", mono)
	}
}

func TestNewClockNilDefaults(t *testing.T) {
	c := NewClock(nil, nil)
	if _, synced := c.Now(); synced {
		t.Fatal("nil systemSynced must mean not synced")
	}
	if err := c.Sync(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), 0); err != nil {
		t.Fatal(err)
	}
	if st := c.Status(); st.Source != SourceBrowser || st.SyncAge < 0 {
		t.Fatalf("status = %+v", st)
	}
}

// Race time must never step backward, even with Sync racing Status.
func TestClockNeverNegativeAgeUnderConcurrency(t *testing.T) {
	c := NewClock(time.Now, nil)
	sync0 := time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC)
	if err := c.Sync(sync0, 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			_ = c.Sync(sync0, 0)
		}
	}()
	for i := 0; i < 2000; i++ {
		if st := c.Status(); st.SyncAge < 0 {
			t.Fatalf("negative SyncAge %v", st.SyncAge)
		}
	}
	wg.Wait()
}

func TestClockConcurrentAccess(t *testing.T) {
	c, ft := newTestClock(false)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = c.Sync(time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC), 0)
		}()
		go func() {
			defer wg.Done()
			ft.Advance(time.Millisecond)
			c.Now()
			c.Status()
		}()
	}
	wg.Wait()
}
