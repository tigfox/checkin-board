package linkcheck

import (
	"errors"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

func TestCancelMidRunStopsProbes(t *testing.T) {
	w, cp, _ := pair(t, 21, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{}, w.clock.Now())
	for range 12 {
		w.step()
	}
	if ok, _ := cp.st.CancelLinkCheck(ctx, req.ID, "operator"); !ok {
		t.Fatal("cancel")
	}
	sent := len(cp.gw.TransmissionsWithPrefix("RC1 P"))
	for range 120 {
		w.step()
	}
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckCancelled || len(cp.gw.TransmissionsWithPrefix("RC1 P")) != sent {
		t.Fatalf("after cancel: %+v, probes %d -> %d", c, sent, len(cp.gw.TransmissionsWithPrefix("RC1 P")))
	}
	// It transmitted, so it still counts for the 2-minute spacing.
	var soon *TooSoonError
	if _, err := Request(ctx, cp.st, Req{}, c.RequestedAt.Add(time.Minute)); !errors.As(err, &soon) {
		t.Fatalf("request right after a cancelled run that transmitted: %v", err)
	}
}

func TestStaleRequestExpires(t *testing.T) {
	w, cp, _ := pair(t, 22, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{}, w.clock.Now())
	w.clock.advance(10 * time.Minute) // the service was down
	w.step()
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckCancelled || w.radio.Sent() != 0 {
		t.Fatalf("stale request: %+v, %d frames", c, w.radio.Sent())
	}
}

func TestStallKeepsSpacingThenGivesUp(t *testing.T) {
	w, cp, _ := pair(t, 23, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{Count: 5}, w.clock.Now())
	w.step()
	w.step()                          // probe 1 out
	w.clock.advance(35 * time.Second) // the service stalls past probes 2-4
	w.step()
	w.step()
	w.step()
	if n := len(cp.gw.TransmissionsWithPrefix("RC1 P")); n != 2 {
		t.Fatalf("after a stall: %d probes, want one more, not a burst", n)
	}
	w.clock.advance(10 * time.Minute) // a long stall: the run is abandoned
	w.step()
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckCancelled {
		t.Fatalf("stalled run = %+v", c)
	}
}

func TestRaceStateChangeStopsRun(t *testing.T) {
	w, cp, _ := pair(t, 24, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{}, w.clock.Now())
	for range 3 {
		w.step()
	}
	_, _ = cp.st.SetRaceState(ctx, []string{store.RaceSetup}, store.RaceSecured, nil)
	for range 60 {
		w.step()
	}
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckCancelled || len(cp.gw.TransmissionsWithPrefix("RC1 P")) != 1 {
		t.Fatalf("after securing: %+v, %d probes", c, len(cp.gw.TransmissionsWithPrefix("RC1 P")))
	}
}
