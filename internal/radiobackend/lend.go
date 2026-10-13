package radiobackend

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Lending the radio: UV Studio's bridge and the firmware flasher need
// the radio's serial port to themselves. While it's lent the backend has
// no session and doesn't reconnect; rows wait, and the hold says why.

// lendWait bounds how long Lend waits for the session to let go.
const lendWait = 5 * time.Second

// ErrLent: the radio is already lent.
var ErrLent = errors.New("the radio is already in use")

// Lend closes the session and stops reconnecting until the returned
// func is called (once; later calls do nothing). reason says who has the
// radio, e.g. "UV Studio".
func (b *Backend) Lend(ctx context.Context, reason string) (giveBack func(), err error) {
	b.mu.Lock()
	if b.lent != "" {
		lent := b.lent
		b.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrLent, lent)
	}
	b.lent, b.returned = reason, make(chan struct{})
	sess := b.sess
	b.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	b.setDown(errors.New("radio in use: " + reason))
	b.log.Info("radio: lent", "to", reason)
	deadline := time.Now().Add(lendWait)
	for b.busy() { // a dial in progress finishes and lets go
		if time.Now().After(deadline) {
			b.giveBack()
			return nil, errors.New("the radio didn't let go of its port")
		}
		if err := sleep(ctx, 20*time.Millisecond); err != nil {
			b.giveBack()
			return nil, err
		}
	}
	done := false
	return func() {
		if !done {
			done = true
			b.giveBack()
		}
	}, nil
}

func (b *Backend) giveBack() {
	b.mu.Lock()
	reason := b.lent
	if b.returned != nil {
		close(b.returned)
	}
	b.lent, b.returned = "", nil
	b.connErr = errors.New("radio not connected yet")
	b.mu.Unlock()
	b.log.Info("radio: given back", "by", reason)
}

// Lent says who has the radio ("": nobody).
func (b *Backend) Lent() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lent
}

// busy: a session is up or a dial is in progress.
func (b *Backend) busy() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sess != nil || b.dialing
}

// waitReturned blocks while the radio is lent.
func (b *Backend) waitReturned(ctx context.Context) error {
	b.mu.Lock()
	ch := b.returned
	b.mu.Unlock()
	if ch == nil {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
