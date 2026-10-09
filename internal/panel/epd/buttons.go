package epd

import (
	"context"
	"time"
)

// Button is a press of one of the bonnet's buttons.
type Button int

// The buttons.
const (
	Top Button = iota
	Bottom
)

// Button timing.
const (
	// PollEvery is how often polled buttons are read (the fallback when
	// a kernel's GPIO edge events don't work; edges cost no CPU, polling
	// about 1% of a Pi Zero).
	PollEvery = 50 * time.Millisecond
	// debounce ignores contact bounce after a press.
	debounce = 150 * time.Millisecond
)

// PollButtons reads the two buttons (read reports whether each is held
// down) every interval and sends a press when one goes down. A held or
// stuck button is one press; bounce within the debounce time is ignored.
// It returns when ctx ends.
func PollButtons(ctx context.Context, read func() (top, bottom bool), every time.Duration, now func() time.Time, out chan<- Button) {
	t := time.NewTicker(every)
	defer t.Stop()
	var was [2]bool
	var last [2]time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		top, bottom := read()
		at := now()
		for i, down := range [2]bool{top, bottom} {
			if down && !was[i] && (last[i].IsZero() || at.Sub(last[i]) >= debounce) {
				last[i] = at
				select {
				case out <- Button(i):
				case <-ctx.Done():
					return
				}
			}
			was[i] = down
		}
	}
}
