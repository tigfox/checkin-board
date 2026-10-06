package linkcheck

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

func apiErr(code int) error {
	return &graywolf.APIError{Method: "POST", Path: "/api/messages", StatusCode: code, Message: http.StatusText(code)}
}

func TestNewNeedsDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("New without store accepted")
	}
}

func TestRefusedProbeCountsAsLost(t *testing.T) {
	w, cp, _ := pair(t, 7, gwfake.Profile{})
	cp.gw.FailNextSend(apiErr(http.StatusBadRequest), apiErr(http.StatusBadRequest))
	req, _ := Request(ctx, cp.st, Req{Count: 3}, w.clock.Now())
	c := w.runUntilDone(cp, req.ID, 5*time.Minute)
	if c.RoundTrip != 1 || c.Uplink != 1 || c.Error == "" || c.Verdict != Fail {
		t.Fatalf("result = %+v", c)
	}
}

func TestStartCancelledWhenNodeChanged(t *testing.T) {
	w, cp, _ := pair(t, 8, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{}, w.clock.Now())
	cfg, _ := cp.st.GetSettings(ctx)
	cfg.HQCall = "N1NEW"
	if _, err := cp.st.UpdateSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	c := w.runUntilDone(cp, req.ID, time.Minute)
	if c.State != store.LinkCheckCancelled || c.Error != "the HQ callsign changed" {
		t.Fatalf("check = %+v", c)
	}
	// A check requested before the race ended doesn't start after it.
	req, _ = Request(ctx, cp.st, Req{}, w.clock.Now().Add(MinInterval))
	_, _ = cp.st.SetRaceState(ctx, []string{store.RaceSetup}, store.RaceSecured, nil)
	c = w.runUntilDone(cp, req.ID, time.Minute)
	if c.State != store.LinkCheckCancelled {
		t.Fatalf("check = %+v", c)
	}
}

func TestReplyResendConflictAndGone(t *testing.T) {
	w, cp, hq := pair(t, 9, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{Count: 1}, w.clock.Now())
	w.step()
	w.step() // HQ heard probe 1/1 and replied
	w.radio.SetDown(true)
	hq.gw.FailNextResend(apiErr(http.StatusConflict), apiErr(http.StatusNotFound))
	w.runUntilDone(cp, req.ID, 5*time.Minute)
	for range 120 {
		w.step()
	}
	rs, _ := hq.st.ListLinkResponses(ctx, 10)
	if len(rs) != 1 || rs[0].ReplyDueAt != nil {
		t.Fatalf("response = %+v", rs)
	}
	// First send; a 409 (still going out) counts as a try; a 404 is
	// sent as new; then the three tries are used up.
	if n := len(hq.gw.TransmissionsWithPrefix("RC1 Q")); n != 2 {
		t.Fatalf("reply transmissions = %d", n)
	}
}

func TestReplyBudgetCapsSpoofedProbes(t *testing.T) {
	w := newWorld(t, 10, gwfake.Profile{})
	hq := w.add("N0HQ", hqSettings(store.RaceSetup))
	// Forged probes from many "stations", each a one-probe run.
	for i := range replyBudget + 5 {
		call := "W" + string(rune('A'+i%26)) + string(rune('A'+i/26)) + "X"
		w.radio.Inject(gwfake.Frame{From: call, To: "N0HQ", Text: "RC1 P ZZ9 7 1/1", MsgID: "1"})
	}
	for range 10 {
		w.step()
	}
	if n := len(hq.gw.TransmissionsWithPrefix("RC1 Q")); n != replyBudget {
		t.Fatalf("replies = %d, want the hourly budget %d", n, replyBudget)
	}
}

func TestFinishCatchesAcksTheFeedMissed(t *testing.T) {
	w, cp, _ := pair(t, 11, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{Count: 1}, w.clock.Now())
	w.step()
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	// Run finish directly before the feed delivered the ACK.
	w.radio.Deliver()
	w.clock.advance(time.Second)
	w.radio.Deliver()
	if err := cp.svc.finish(ctx, c, w.clock.Now()); err != nil {
		t.Fatal(err)
	}
	c, _ = cp.st.GetLinkCheck(ctx, req.ID)
	if c.RoundTrip != 1 || c.State != store.LinkCheckDone {
		t.Fatalf("check = %+v", c)
	}
}

func TestStrayRepliesIgnored(t *testing.T) {
	w, cp, _ := pair(t, 12, gwfake.Profile{})
	// No running check: a reply is ignored.
	w.radio.Inject(gwfake.Frame{From: "N0HQ", To: "K1CP", Text: "RC1 Q AS5 7 1-5 -20 -", MsgID: "2"})
	w.step()
	req, _ := Request(ctx, cp.st, Req{Count: 2}, w.clock.Now())
	w.step()
	// A reply for another run, and one from another station, are ignored.
	w.radio.Inject(gwfake.Frame{From: "N0HQ", To: "K1CP", Text: "RC1 Q AS5 1 1-2 -20 -", MsgID: "3"})
	w.radio.Inject(gwfake.Frame{From: "W9XX", To: "K1CP", Text: "RC1 Q AS5 1 1-2 -20 -", MsgID: "4"})
	c := w.runUntilDone(cp, req.ID, 5*time.Minute)
	if c.Verdict != Pass {
		t.Fatalf("check = %+v", c)
	}
}

func TestRunTicksUntilCancelled(t *testing.T) {
	_, cp, _ := pair(t, 13, gwfake.Profile{})
	c, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	if err := cp.svc.Run(c); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run = %v", err)
	}
}

func TestViaFieldAndHelpers(t *testing.T) {
	if v := viaField("wide1-1*"); v != "WIDE1-1" {
		t.Errorf("via = %q", v)
	}
	if v := viaField("BAD CALL!"); v != "" {
		t.Errorf("via = %q", v)
	}
	if m := medianInt([]int{-30, -10}); *m != -20 {
		t.Errorf("median = %d", *m)
	}
	if clampLevel(-140) != -99 || clampLevel(3) != 0 {
		t.Error("clamp")
	}
}
