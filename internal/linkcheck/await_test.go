package linkcheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/store"
)

var quick = Timing{Poll: 5 * time.Millisecond, StartWait: 50 * time.Millisecond, MaxWait: 150 * time.Millisecond}

func TestAwaitCancelsWhenNotPickedUp(t *testing.T) {
	st := openStore(t, cpSettings(store.RaceSetup))
	c, _ := Request(ctx, st, Req{}, time.Now())
	if _, err := Await(ctx, st, c.ID, quick); !errors.Is(err, ErrNotPickedUp) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := st.GetLinkCheck(ctx, c.ID); got.State != store.LinkCheckCancelled {
		t.Fatalf("check = %+v", got)
	}
}

func TestAwaitTimesOutAndHonoursContext(t *testing.T) {
	st := openStore(t, cpSettings(store.RaceSetup))
	c, _ := Request(ctx, st, Req{}, time.Now())
	now := time.Now()
	c.State, c.StartedAt = store.LinkCheckRunning, &now
	_ = st.UpdateLinkCheck(ctx, c)
	if _, err := Await(ctx, st, c.ID, quick); err == nil || !strings.Contains(err.Error(), "no result") {
		t.Fatalf("err = %v", err)
	}
	c2, _ := Request(ctx, st, Req{}, time.Now().Add(MinInterval))
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := Await(cctx, st, c2.ID, quick); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := st.GetLinkCheck(ctx, c2.ID); got.State != store.LinkCheckCancelled {
		t.Fatalf("check = %+v", got)
	}
}

func TestBrief(t *testing.T) {
	rtt, r, l := 4400, -21, -22
	c := store.LinkCheck{State: store.LinkCheckDone, Verdict: Pass, PeerCall: "N0HQ", Count: 5, Uplink: 5, RoundTrip: 5,
		MedianRTTms: &rtt, RemoteLevel: &r, LocalLevel: &l}
	if b := Brief(c); b != "PASS N0HQ up5/5 ack5 rtt4s -21/-22dB" || len(b) > 50 {
		t.Errorf("brief = %q", b)
	}
	c.LocalLevel = nil
	if b := Brief(c); !strings.HasSuffix(b, "-21/?dB") {
		t.Errorf("brief = %q", b)
	}
	if b := Brief(store.LinkCheck{State: store.LinkCheckCancelled, Error: "x"}); b != "cancelled: x" {
		t.Errorf("brief = %q", b)
	}
}
