package epd

import (
	"context"
	"testing"
	"time"
)

// A scripted pair of active-low buttons: each step is (top, bottom)
// levels for one poll; true means pressed (pin low).
func TestPollButtonsDetectsPressesAndDebounces(t *testing.T) {
	steps := [][2]bool{
		{false, false}, {true, false}, {true, false}, {false, false}, // top press
		{false, true}, {false, false}, {false, true}, {false, false}, // bottom press, then a bounce
		{false, false}, {false, false}, {false, false}, {false, false},
		{false, true}, {false, true}, {false, false}, // bottom again, after the debounce
	}
	i := 0
	read := func() (bool, bool) {
		if i >= len(steps) {
			return false, false
		}
		s := steps[i]
		i++
		return s[0], s[1]
	}
	now := time.Unix(0, 0)
	clock := func() time.Time { now = now.Add(50 * time.Millisecond); return now }
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan Button, 10)
	done := make(chan struct{})
	go func() {
		PollButtons(ctx, read, time.Microsecond, clock, out)
		close(done)
	}()
	var got []Button
	for len(got) < 3 {
		select {
		case b := <-out:
			got = append(got, b)
		case <-time.After(2 * time.Second):
			t.Fatalf("presses = %v", got)
		}
	}
	cancel()
	<-done
	if got[0] != Top || got[1] != Bottom || got[2] != Bottom {
		t.Fatalf("presses = %v, want top, bottom, bottom (bounce ignored)", got)
	}
	select {
	case b := <-out:
		t.Fatalf("extra press %v", b)
	default:
	}
}

func TestPollButtonsHeldIsOnePress(t *testing.T) {
	read := func() (bool, bool) { return true, false } // held down (or stuck)
	now := time.Unix(0, 0)
	clock := func() time.Time { now = now.Add(20 * time.Millisecond); return now }
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	out := make(chan Button, 100)
	PollButtons(ctx, read, time.Millisecond, clock, out)
	if len(out) != 1 {
		t.Fatalf("a held button gave %d presses, want 1", len(out))
	}
}
