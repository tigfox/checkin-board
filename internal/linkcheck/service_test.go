package linkcheck

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/inbox"
	"checkin-board/internal/peers"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// router is the link-check slice of the app's dispatcher.
type router struct{ svc *Service }

func (r router) HandleInbound(ctx2 context.Context, m graywolf.Message) error {
	msg, err := wire.Decode(m.Text)
	if err != nil {
		return nil
	}
	return r.svc.HandleInbound(ctx2, m, msg)
}

func (r router) HandleOutbound(ctx2 context.Context, m graywolf.Message) error {
	if _, err := wire.Decode(m.Text); err != nil {
		return nil
	}
	return r.svc.HandleOutbound(ctx2, m)
}

type node struct {
	call  string
	st    *store.Store
	gw    *gwfake.Station
	svc   *Service
	inbox *inbox.Reader
}

type world struct {
	t     *testing.T
	clock *clock
	radio *gwfake.Radio
	nodes []*node
}

func newWorld(t *testing.T, seed uint64, prof gwfake.Profile) *world {
	c := &clock{t: t0}
	return &world{t: t, clock: c, radio: gwfake.NewRadio(seed, prof, c.Now)}
}

func (w *world) add(call string, cfg store.Settings) *node {
	w.t.Helper()
	st := openStore(w.t, cfg)
	st.SetClock(w.clock.Now)
	gw := gwfake.New(call)
	gw.Now = w.clock.Now
	w.radio.Attach(gw)
	svc, err := New(Config{Store: st, Graywolf: gw, Peers: peers.NewEnsurer(gw, st, w.clock.Now), Now: w.clock.Now})
	if err != nil {
		w.t.Fatal(err)
	}
	in, err := inbox.New(inbox.Config{Graywolf: gw, Store: st, Dispatcher: router{svc}})
	if err != nil {
		w.t.Fatal(err)
	}
	n := &node{call: call, st: st, gw: gw, svc: svc, inbox: in}
	w.nodes = append(w.nodes, n)
	return n
}

func (w *world) step() {
	w.clock.advance(time.Second)
	w.radio.Deliver()
	for _, n := range w.nodes {
		_ = n.inbox.CatchUp(ctx)
		if err := n.svc.Tick(ctx); err != nil {
			w.t.Logf("%s tick: %v", n.call, err)
		}
	}
}

// runUntilDone steps until n's check id finishes, up to limit.
func (w *world) runUntilDone(n *node, id uint, limit time.Duration) store.LinkCheck {
	w.t.Helper()
	for end := w.clock.Now().Add(limit); w.clock.Now().Before(end); {
		w.step()
		c, err := n.st.GetLinkCheck(ctx, id)
		if err != nil {
			w.t.Fatal(err)
		}
		if c.State == store.LinkCheckDone || c.State == store.LinkCheckCancelled {
			return c
		}
	}
	w.t.Fatalf("check %d not done after %s", id, limit)
	return store.LinkCheck{}
}

func pair(t *testing.T, seed uint64, prof gwfake.Profile) (*world, *node, *node) {
	w := newWorld(t, seed, prof)
	cp := w.add("N0CALL-1", cpSettings(store.RaceSetup))
	hq := w.add("N0CALL-10", hqSettings(store.RaceSetup))
	return w, cp, hq
}

func TestCleanLinkPasses(t *testing.T) {
	w, cp, hq := pair(t, 1, gwfake.Profile{MaxDelay: 3 * time.Second})
	hq.gw.SetRXLevel("N0CALL-1", -21)
	cp.gw.SetRXLevel("N0CALL-10", -3) // HQ is too loud at the checkpoint
	req, err := Request(ctx, cp.st, Req{Source: "admin"}, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := w.runUntilDone(cp, req.ID, 5*time.Minute)
	if c.Verdict != Pass || c.Uplink != 5 || c.RoundTrip != 5 || !c.ReplyReceived || c.MedianRTTms == nil {
		t.Fatalf("result = %+v", c)
	}
	if c.RemoteLevel == nil || *c.RemoteLevel != -21 || c.LocalLevel == nil || *c.LocalLevel != -3 {
		t.Fatalf("levels: remote %v local %v", c.RemoteLevel, c.LocalLevel)
	}
	if !strings.Contains(c.Advice, "too hot") {
		t.Fatalf("advice = %q", c.Advice)
	}
	// The run took about 4 spacings plus the ACK of the last probe, not
	// the full reply timeout.
	if took := c.FinishedAt.Sub(*c.StartedAt); took > 70*time.Second {
		t.Fatalf("run took %s", took)
	}
	// HQ kept what it heard, and its reply was ACKed.
	for range 10 {
		w.step()
	}
	rs, _ := hq.st.ListLinkResponses(ctx, 10)
	if len(rs) != 1 || rs[0].Heard != "1,2,3,4,5" || rs[0].ReplyAckedAt == nil || rs[0].UnknownPeer {
		t.Fatalf("HQ responses = %+v", rs)
	}
	// One frame per probe and one reply: graywolf's retries are off.
	if n := len(cp.gw.TransmissionsWithPrefix("RC1 P")); n != 5 {
		t.Fatalf("probe transmissions = %d", n)
	}
	if n := len(hq.gw.TransmissionsWithPrefix("RC1 Q")); n != 1 {
		t.Fatalf("reply transmissions = %d", n)
	}
	// Every graywolf row the check made is recorded for cleanup.
	rows, _ := cp.st.ListGWRows(ctx)
	kinds := map[string]int{}
	for _, r := range rows {
		kinds[r.Kind]++
	}
	if kinds[store.GWRowProbe] != 5 || kinds[store.GWRowInbound] != 1 {
		t.Fatalf("checkpoint gw rows = %v", kinds)
	}
}

func TestDeadLinkFailsWithinBoundedAirtime(t *testing.T) {
	w, cp, _ := pair(t, 1, gwfake.Profile{})
	w.radio.SetDown(true)
	req, _ := Request(ctx, cp.st, Req{}, w.clock.Now())
	c := w.runUntilDone(cp, req.ID, 10*time.Minute)
	if c.Verdict != Fail || c.ReplyReceived || c.RoundTrip != 0 || !strings.Contains(c.Advice, "not usable") {
		t.Fatalf("result = %+v", c)
	}
	if n := w.radio.Sent(); n != 5 {
		t.Fatalf("frames on air = %d, want the 5 probes only", n)
	}
}

func TestHQProbesCheckpointAndFlagsUnknownPeers(t *testing.T) {
	w, cp, hq := pair(t, 2, gwfake.Profile{})
	req, err := Request(ctx, hq.st, Req{To: "N0CALL-1"}, w.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	c := w.runUntilDone(hq, req.ID, 5*time.Minute)
	if c.Verdict != Pass {
		t.Fatalf("HQ → CP = %+v", c)
	}
	rs, _ := cp.st.ListLinkResponses(ctx, 10)
	if len(rs) != 1 || rs[0].ProberCode != HQCode {
		t.Fatalf("checkpoint responses = %+v", rs)
	}
	// HQ answers a station it doesn't know, but flags it.
	stranger := w.add("N0CALL-14", cpSettings(store.RaceSetup))
	cfg, _ := stranger.st.GetSettings(ctx)
	cfg.CheckpointCode = "ZZ9"
	_, _ = stranger.st.UpdateSettings(ctx, cfg)
	if err := hq.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Ridge", CourseOrder: 1, ExpectedCall: "N0CALL-1"}); err != nil {
		t.Fatal(err)
	}
	r2, _ := Request(ctx, stranger.st, Req{Count: 2}, w.clock.Now())
	w.runUntilDone(stranger, r2.ID, 5*time.Minute)
	rs, _ = hq.st.ListLinkResponses(ctx, 10)
	if len(rs) != 1 || rs[0].PeerCall != "N0CALL-14" || !rs[0].UnknownPeer {
		t.Fatalf("HQ responses = %+v", rs)
	}
}

func TestCheckpointAnswersOnlyItsHQ(t *testing.T) {
	w, cp, _ := pair(t, 3, gwfake.Profile{})
	rogue := w.add("N0CALL-14", hqSettings(store.RaceSetup))
	req, _ := Request(ctx, rogue.st, Req{To: "N0CALL-1", Count: 2}, w.clock.Now())
	c := w.runUntilDone(rogue, req.ID, 5*time.Minute)
	if c.ReplyReceived {
		t.Fatalf("checkpoint answered a non-HQ station: %+v", c)
	}
	if rs, _ := cp.st.ListLinkResponses(ctx, 10); len(rs) != 0 {
		t.Fatalf("responses = %+v", rs)
	}
}

func TestResponderQuietOutsideSetupAndActive(t *testing.T) {
	w, cp, hq := pair(t, 4, gwfake.Profile{})
	if ok, err := cp.st.SetRaceState(ctx, []string{store.RaceSetup}, store.RaceSecured, nil); !ok || err != nil {
		t.Fatal(ok, err)
	}
	req, _ := Request(ctx, hq.st, Req{To: "N0CALL-1", Count: 2}, w.clock.Now())
	c := w.runUntilDone(hq, req.ID, 5*time.Minute)
	if c.ReplyReceived {
		t.Fatal("a secured checkpoint transmitted a reply")
	}
}

func TestReplyResentWhenNotAcked(t *testing.T) {
	w, cp, hq := pair(t, 5, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{Count: 1}, w.clock.Now())
	// Let the probe through, then cut the link so the reply's ACK never
	// comes back: the responder resends twice, then gives up.
	w.step()
	w.step()
	w.radio.SetDown(true)
	w.runUntilDone(cp, req.ID, 10*time.Minute)
	w.clock.advance(0)
	for range 120 {
		w.step()
	}
	rs, _ := hq.st.ListLinkResponses(ctx, 10)
	if len(rs) != 1 || rs[0].ReplyAttempts != 3 || rs[0].ReplyDueAt != nil {
		t.Fatalf("response = %+v", rs)
	}
}

func TestCancelRequested(t *testing.T) {
	_, cp, _ := pair(t, 6, gwfake.Profile{})
	req, _ := Request(ctx, cp.st, Req{}, t0)
	ok, err := cp.st.CancelLinkCheck(ctx, req.ID, "cancelled by the operator")
	if !ok || err != nil {
		t.Fatal(ok, err)
	}
	c, _ := cp.st.GetLinkCheck(ctx, req.ID)
	if c.State != store.LinkCheckCancelled || c.Error == "" {
		t.Fatalf("check = %+v", c)
	}
}
