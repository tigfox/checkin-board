package linkcheck

import (
	"context"
	"errors"
	"fmt"
	"time"

	"checkin-board/internal/store"
)

// Timing bounds a caller's wait for a run it requested.
type Timing struct {
	Poll      time.Duration
	StartWait time.Duration // the service must pick the run up by then
	MaxWait   time.Duration
}

// DefaultTiming suits a person or script waiting for one run.
var DefaultTiming = Timing{Poll: time.Second, StartWait: 20 * time.Second, MaxWait: 10 * time.Minute}

// ErrNotPickedUp: the service never started the run (is it running?).
var ErrNotPickedUp = errors.New("linkcheck: the checkin-board service didn't start the check: is it running? (systemctl status checkin-board)")

// Await polls the store until run id finishes, and cancels it if the
// service doesn't start it, the wait times out, or ctx ends.
func Await(ctx context.Context, st *store.Store, id uint, tm Timing) (store.LinkCheck, error) {
	start := time.Now()
	t := time.NewTicker(tm.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			_, _ = st.CancelLinkCheck(context.WithoutCancel(ctx), id, "the requester gave up")
			return store.LinkCheck{}, ctx.Err()
		case <-t.C:
		}
		cur, err := st.GetLinkCheck(ctx, id)
		if err != nil {
			return cur, err
		}
		switch {
		case cur.State == store.LinkCheckDone || cur.State == store.LinkCheckCancelled:
			return cur, nil
		case cur.State == store.LinkCheckRequested && time.Since(start) > tm.StartWait:
			if ok, _ := st.CancelLinkCheck(ctx, id, "not picked up"); ok {
				return cur, ErrNotPickedUp
			}
		case time.Since(start) > tm.MaxWait:
			_, _ = st.CancelLinkCheck(ctx, id, "timed out")
			return cur, fmt.Errorf("linkcheck: no result after %s", tm.MaxWait)
		}
	}
}

// Brief fits a result in a graywolf Action's ~50-character on-air reply:
// "PASS N0CALL-10 up5/5 ack5 rtt4s -21/-22dB".
func Brief(c store.LinkCheck) string {
	if c.State == store.LinkCheckCancelled {
		return "cancelled: " + c.Error
	}
	s := fmt.Sprintf("%s %s up%d/%d ack%d", c.Verdict, c.PeerCall, c.Uplink, c.Count, c.RoundTrip)
	if c.MedianRTTms != nil {
		s += fmt.Sprintf(" rtt%ds", (*c.MedianRTTms+500)/1000)
	}
	lvl := func(v *int) string {
		if v == nil {
			return "?"
		}
		return fmt.Sprint(*v)
	}
	if c.RemoteLevel != nil || c.LocalLevel != nil {
		s += fmt.Sprintf(" %s/%sdB", lvl(c.RemoteLevel), lvl(c.LocalLevel))
	}
	return s
}
