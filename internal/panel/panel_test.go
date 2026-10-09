package panel

import (
	"context"
	"errors"
	"image"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/panel/epd"
	"checkin-board/internal/panel/menu"
)

var ctx = context.Background()

// fakeDisplay records what was drawn.
type fakeDisplay struct {
	name    string
	partial bool
	fail    bool // Full returns an error (a BUSY timeout, an SPI fault)
	tries   int
	full    []image.Image
	parts   []image.Image
	closed  bool
}

func (d *fakeDisplay) Full(img image.Image) error {
	d.tries++
	if d.fail {
		return errors.New("busy timeout")
	}
	d.full = append(d.full, img)
	return nil
}
func (d *fakeDisplay) Partial(img image.Image) error { d.parts = append(d.parts, img); return nil }
func (d *fakeDisplay) CanPartial() bool              { return d.partial }
func (d *fakeDisplay) Close() error                  { d.closed = true; return nil }

// fakeApp serves views and records calls.
type fakeApp struct {
	mu         sync.Mutex
	view       View
	err        error
	acts       []ActionRequest
	actIDs     []int64
	refreshed  []time.Time
	controller []string
	gaveUp     int
}

func (a *fakeApp) DetectGaveUp(context.Context) error {
	a.gaveUp++
	a.view.Settings.DetectPending = false
	return nil
}

func (a *fakeApp) View(context.Context) (View, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.view, a.err
}

func (a *fakeApp) Act(_ context.Context, id int64, r ActionRequest) (ActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.actIDs, a.acts = append(a.actIDs, id), append(a.acts, r)
	return ActionResult{OK: true, Message: "Done"}, nil
}

func (a *fakeApp) Refreshed(_ context.Context, at time.Time) error {
	a.refreshed = append(a.refreshed, at)
	allowed := at.Add(floor)
	a.view.FullAllowedAt = &allowed // as the app does
	return nil
}

func (a *fakeApp) SetController(_ context.Context, c string) error {
	a.controller = append(a.controller, c)
	a.view.Settings.Controller, a.view.Settings.DetectPending = c, false
	return nil
}

type rig struct {
	t      *testing.T
	now    time.Time
	app    *fakeApp
	opened map[string]*fakeDisplay
	order  []string
	p      *Panel
}

func items(role string) []MenuItem {
	var out []MenuItem
	for i, it := range menu.Defaults() {
		it.ID = int64(i + 1)
		it.Confirm = it.Confirm || it.Action.Lifecycle()
		out = append(out, MenuItem{Item: it, Available: it.Available(role, "active")})
	}
	return out
}

func newRig(t *testing.T, partial bool) *rig {
	v := cpView()
	v.Settings = Settings{Enabled: true, Controller: epd.SSD1680Z, RefreshMin: 5}
	v.Menu = items("checkpoint")
	r := &rig{t: t, now: tNow, app: &fakeApp{view: v}, opened: map[string]*fakeDisplay{}}
	r.start(partial)
	return r
}

// start (re)starts the panel process, as after a crash.
func (r *rig) start(partial bool) {
	r.p = New(Config{
		Source: r.app,
		Open: func(c string) (epd.Display, error) {
			// One handle on the hardware at a time.
			for name, d := range r.opened {
				if !d.closed {
					r.t.Errorf("opening %s while %s is still open", c, name)
				}
			}
			r.order = append(r.order, c)
			d := &fakeDisplay{name: c, partial: partial}
			r.opened[c] = d
			return d, nil
		},
		Addrs: func() []string { return []string{"192.168.4.1"} },
		Now:   func() time.Time { return r.now },
	})
}

func (r *rig) disp() *fakeDisplay { return r.opened[epd.SSD1680Z] }

// run advances the clock in small steps, ticking the panel.
func (r *rig) run(d time.Duration) {
	for end := r.now.Add(d); r.now.Before(end); {
		r.now = r.now.Add(250 * time.Millisecond)
		r.p.Tick(ctx)
	}
}

func (r *rig) press(b Button) {
	r.p.Press(ctx, b)
	r.run(time.Second)
}

// shown is the text-free check: the last image drawn equals want.
func sameImage(a, b image.Image) bool {
	ga, gb := a.(*image.Gray), b.(*image.Gray)
	return string(ga.Pix) == string(gb.Pix)
}

func TestStatusRefreshCadenceAndFloor(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	if n := len(r.disp().full); n != 1 || len(r.app.refreshed) != 1 {
		t.Fatalf("first draw: %d full, %d reported", n, len(r.app.refreshed))
	}
	// Nothing changed: no refresh, even after the interval.
	r.run(6 * time.Minute)
	if n := len(r.disp().full); n != 1 {
		t.Fatalf("refreshed with nothing changed: %d", n)
	}
	// A routine change waits for the interval (5 min, long passed here):
	// it shows at the next poll.
	r.app.view.Status.Unconfirmed = 4
	r.run(35 * time.Second)
	if n := len(r.disp().full); n != 2 {
		t.Fatalf("changed status not drawn at the interval: %d", n)
	}
	// An important change (race state) comes early, but never inside 3
	// min of the last refresh (just now).
	r.app.view.Status.RaceState, r.app.view.Status.StateLabel = "complete", "Checkpoint closed"
	r.run(2 * time.Minute)
	if n := len(r.disp().full); n != 2 {
		t.Fatalf("refreshed inside the 3-minute floor: %d", n)
	}
	r.run(90 * time.Second)
	if n := len(r.disp().full); n != 3 {
		t.Fatalf("important change not drawn at the floor: %d", n)
	}
	// The app's floor (remembered across panel restarts) is obeyed too.
	later := r.now.Add(10 * time.Minute)
	r.app.view.FullAllowedAt = &later
	r.app.view.Status.Unconfirmed = 9
	r.app.view.RefreshSeq++
	r.run(8 * time.Minute)
	if n := len(r.disp().full); n != 3 {
		t.Fatalf("refreshed before the app's allowed time: %d", n)
	}
	r.run(3 * time.Minute)
	if n := len(r.disp().full); n != 4 {
		t.Fatalf("not refreshed after the app's allowed time: %d", n)
	}
}

func TestAppDownAtStartWaitsForTheFloor(t *testing.T) {
	r := newRig(t, true)
	r.app.err = errors.New("connection refused")
	r.p.Tick(ctx) // no view: the display can't even be opened yet
	r.run(2 * time.Minute)
	if len(r.order) != 0 {
		// Without a view the controller is unknown; nothing is drawn.
		t.Fatalf("opened %v with no view", r.order)
	}
	r.app.err = nil
	r.run(time.Minute)
	if len(r.disp().full) != 1 {
		t.Fatal("status not drawn once the app answered")
	}
	// The app goes away later: the screen says so at the next allowed refresh.
	r.app.err = errors.New("connection refused")
	r.run(6 * time.Minute)
	last := r.disp().full[len(r.disp().full)-1]
	if !sameImage(last, AppDownScreen("connection refused", r.now)) && len(r.disp().full) != 2 {
		t.Fatalf("app-down screen not shown: %d draws", len(r.disp().full))
	}
}

func TestMenuNavigateConfirmAndRun(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.press(Top) // opens the menu
	if len(r.disp().parts) == 0 {
		t.Fatal("menu not drawn with a partial refresh")
	}
	// Available in "active" for a checkpoint: Status, Run link check,
	// Close checkpoint, Secure for travel, Last link check, Network,
	// Refresh screen, then Back. Move to "Close checkpoint" (index 2).
	r.press(Top)
	r.press(Top)
	r.press(Bottom) // select: needs confirming
	r.press(Top)    // cancel
	if len(r.app.actIDs) != 0 {
		t.Fatal("cancelled action ran")
	}
	r.press(Bottom) // still on Close checkpoint: confirm again
	r.press(Bottom) // yes
	if len(r.app.actIDs) != 1 || r.app.actIDs[0] != 4 {
		t.Fatalf("acted on %v, want item 4 (Close checkpoint)", r.app.actIDs)
	}
	// The result shows for 10 s, then the status screen comes back.
	full := len(r.disp().full)
	r.run(12 * time.Second)
	if len(r.disp().parts)+len(r.disp().full) <= full {
		t.Fatal("status not redrawn after the result")
	}
}

func TestIdleMenuReturnsToStatusAndCleansUp(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.press(Top)
	r.run(31 * time.Second) // idle: back to status (partial)...
	if r.p.mode != modeStatus {
		t.Fatalf("mode = %v after 30 s idle", r.p.mode)
	}
	full := len(r.disp().full)
	r.run(3 * time.Minute) // ...and a full refresh at the floor clears ghosting
	if len(r.disp().full) != full+1 {
		t.Fatalf("no clean-up full refresh: %d -> %d", full, len(r.disp().full))
	}
}

func TestPartialRateLimit(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.p.Press(ctx, Top)
	for range 5 {
		r.p.Press(ctx, Top) // mashing the button
	}
	r.now = r.now.Add(100 * time.Millisecond)
	r.p.Tick(ctx)
	if n := len(r.disp().parts); n > 1 {
		t.Fatalf("%d partial refreshes in a fraction of a second", n)
	}
	r.run(2 * time.Second)
	if r.p.cursor != 5 {
		t.Fatalf("presses lost: cursor %d", r.p.cursor)
	}
}

func TestNoPartialPanelUsesFullRefreshes(t *testing.T) {
	r := newRig(t, false)
	r.run(time.Second)
	r.press(Top)
	if len(r.disp().parts) != 0 || len(r.disp().full) != 2 {
		t.Fatalf("parts %d, full %d", len(r.disp().parts), len(r.disp().full))
	}
}

func TestLocalItems(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.press(Top)
	for range 5 {
		r.press(Top) // to "Network" (index 5)
	}
	r.press(Bottom)
	if r.p.mode != modeMessage || len(r.app.actIDs) != 0 {
		t.Fatalf("network: mode %v, acts %v", r.p.mode, r.app.actIDs)
	}
	if !strings.Contains(strings.Join(r.p.message, " "), "192.168.4.1") {
		t.Fatalf("network screen = %q", r.p.message)
	}
	r.run(11 * time.Second)
	r.press(Top)
	for range 6 {
		r.press(Top) // to "Refresh screen"
	}
	r.press(Bottom) // inside the floor: says when it can
	if !strings.Contains(strings.Join(r.p.message, " "), "min") {
		t.Fatalf("refresh inside the floor = %q", r.p.message)
	}
}

func TestHQLinkCheckPicksATarget(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Status.Role = "hq"
	r.app.view.Menu = items("hq")
	r.app.view.Targets = []Target{{Code: "AS1", Name: "Creek", Call: "N0CALL-1"}, {Code: "AS2", Name: "Ridge", Call: "N0CALL-2"}}
	r.run(time.Second)
	r.press(Top)
	r.press(Top)    // "Run link check"
	r.press(Bottom) // pick a checkpoint
	r.press(Top)    // AS2
	r.press(Bottom) // confirm screen
	r.press(Bottom) // yes
	if len(r.app.acts) != 1 || r.app.acts[0].To != "N0CALL-2" {
		t.Fatalf("acts = %+v", r.app.acts)
	}
}

func TestDisabledAndUnavailableItemsHidden(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Menu[0].Enabled = false
	r.app.view.Menu[0].Available = false
	r.run(time.Second)
	r.press(Top)
	for _, e := range r.p.entries {
		if e.label == "Status" || e.label == "Open checkpoint" {
			t.Fatalf("hidden item shown: %q", e.label)
		}
	}
	if r.p.entries[len(r.p.entries)-1].label != "Back" {
		t.Fatal("no Back entry")
	}
}

func TestPanelDisabledDrawsNothing(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.Enabled = false
	r.run(10 * time.Minute)
	if d := r.disp(); d != nil && len(d.full) > 0 {
		t.Fatal("disabled panel drew")
	}
	r.p.Press(ctx, Top)
	if r.p.mode != modeStatus {
		t.Fatal("disabled panel opened its menu")
	}
}

func TestWizardFindsTheController(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.Controller, r.app.view.Settings.DetectPending = "", true
	r.run(time.Second)
	if len(r.order) != 1 || r.order[0] != epd.SSD1680Z {
		t.Fatalf("wizard opened %v first", r.order)
	}
	r.run(wizardStep) // next candidate
	if len(r.order) != 2 || r.order[1] != epd.SSD1680 || !r.opened[epd.SSD1680Z].closed {
		t.Fatalf("wizard order %v", r.order)
	}
	r.p.Press(ctx, Bottom) // too soon after the switch: the frame may not be up yet
	if len(r.app.controller) != 0 {
		t.Fatal("picked a controller during its own redraw")
	}
	r.run(wizardSettle)
	r.press(Bottom) // readable on SSD1680
	if len(r.app.controller) != 1 || r.app.controller[0] != epd.SSD1680 || r.p.mode != modeStatus {
		t.Fatalf("picked %v, mode %v", r.app.controller, r.p.mode)
	}
}

func TestWizardStopsAfterRoundsAndStaysStopped(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.Controller, r.app.view.Settings.DetectPending = "", true
	r.run(wizardStep * time.Duration(3*wizardRounds+3))
	if n := len(r.order); n != 3*wizardRounds || r.app.gaveUp != 1 {
		t.Fatalf("wizard drew %d times (want %d), gave up %d", n, 3*wizardRounds, r.app.gaveUp)
	}
	// A restarted panel doesn't run it again (the app remembers)...
	r.start(true)
	r.run(10 * time.Minute)
	if n := len(r.order); n != 3*wizardRounds {
		t.Fatalf("wizard reran after a restart: %d draws", n)
	}
	// ...until an admin asks to detect again.
	r.app.view.Settings.DetectPending = true
	r.run(pollEvery + time.Second)
	if n := len(r.order); n <= 3*wizardRounds || r.order[3*wizardRounds] != epd.SSD1680Z {
		t.Fatalf("detect again didn't restart the wizard: %v", r.order)
	}
}

func TestWizardWaitsForTheFloor(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.Controller, r.app.view.Settings.DetectPending = "", true
	later := r.now.Add(2 * time.Minute)
	r.app.view.FullAllowedAt = &later // the panel refreshed just before a restart
	r.run(time.Minute)
	if len(r.order) != 0 {
		t.Fatal("wizard drew inside the floor")
	}
	r.run(90 * time.Second)
	if len(r.order) == 0 || r.order[0] != epd.SSD1680Z {
		t.Fatalf("wizard didn't start after the floor: %v", r.order)
	}
}

func TestFailedRefreshStillRests(t *testing.T) {
	r := newRig(t, true)
	r.p.Tick(ctx) // open the display
	r.disp().fail = true
	r.app.view.RefreshSeq++
	r.run(10 * time.Minute)
	if n := r.disp().tries; n < 1 || n > 4 {
		t.Fatalf("%d attempts in 10 min (one per 3 min at most)", n)
	}
}

func TestStuckButtonIsCapped(t *testing.T) {
	for _, partial := range []bool{false, true} {
		r := newRig(t, partial)
		r.run(time.Second)
		for range 600 { // a stuck or bouncing button for 10 min
			r.p.Press(ctx, Top)
			r.run(time.Second)
		}
		d := r.disp()
		full, parts := len(d.full), len(d.parts)
		if !partial && full > 4*fullBudget+2 {
			t.Errorf("no partial: %d full refreshes in 10 min", full)
		}
		if partial && parts > 4*partialBudget {
			t.Errorf("partial: %d partial refreshes in 10 min", parts)
		}
	}
}

func TestRecoveryFromAppDownRedrawsAtTheFloor(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.RefreshMin = 30
	r.run(time.Second)
	r.app.err = errors.New("connection refused")
	r.run(4 * time.Minute) // down screen at the floor
	n := len(r.disp().full)
	if n != 2 {
		t.Fatalf("down screen: %d draws", n)
	}
	r.app.err = nil
	r.run(4 * time.Minute)
	if len(r.disp().full) != 3 {
		t.Fatal("status not back at the floor after recovery (waited for the 30 min interval)")
	}
}

func TestConfirmIgnoresBounce(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.press(Top)
	r.press(Top)
	r.press(Top)           // "Close checkpoint"
	r.p.Press(ctx, Bottom) // confirm screen
	r.p.Press(ctx, Bottom) // contact bounce: ignored
	if len(r.app.actIDs) != 0 {
		t.Fatal("a bounce confirmed a lifecycle action")
	}
	r.run(time.Second)
	r.press(Bottom)
	if len(r.app.actIDs) != 1 {
		t.Fatal("deliberate confirm ignored")
	}
}

func TestActionCarriesMenuRevision(t *testing.T) {
	r := newRig(t, true)
	r.app.view.MenuRev = 7
	r.run(time.Second)
	r.press(Top)
	r.press(Top)    // Run link check
	r.press(Bottom) // confirm
	r.press(Bottom)
	if len(r.app.acts) != 1 || r.app.acts[0].MenuRev != 7 {
		t.Fatalf("acts = %+v", r.app.acts)
	}
}

func TestAppRestartDoesNotReplayRequests(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Boot, r.app.view.TestPattern, r.app.view.TestPatternSeq = 1, epd.SSD1675, 3
	r.run(time.Second)
	r.app.view.Boot, r.app.view.TestPatternSeq = 2, 1 // the app restarted
	r.run(pollEvery + time.Second)
	if r.opened[epd.SSD1675] != nil {
		t.Fatal("an app restart replayed a test pattern")
	}
}

func TestDisablingLeavesTheMenu(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.press(Top)
	r.app.view.Settings.Enabled = false
	r.run(pollEvery + time.Second)
	if r.p.mode != modeStatus {
		t.Fatalf("mode = %v after disabling", r.p.mode)
	}
}

func TestTestPatternAndRotation(t *testing.T) {
	r := newRig(t, true)
	r.app.view.Settings.Rotation = 180
	r.run(time.Second)
	if !sameImage(r.disp().full[0], Rotate(StatusScreen(r.app.view, []string{"192.168.4.1"}), 180)) {
		t.Fatal("status not rotated")
	}
	r.app.view.TestPattern, r.app.view.TestPatternSeq = epd.SSD1675, 1
	r.run(pollEvery + time.Second)
	if r.opened[epd.SSD1675] != nil {
		t.Fatal("test pattern drawn inside the floor")
	}
	r.run(3 * time.Minute)
	d := r.opened[epd.SSD1675]
	if d == nil || len(d.full) != 1 || !d.closed {
		t.Fatalf("test pattern display = %+v", d)
	}
}

func TestControllerChangeReopens(t *testing.T) {
	r := newRig(t, true)
	r.run(time.Second)
	r.app.view.Settings.Controller = epd.SSD1675
	r.run(pollEvery + time.Second)
	if !r.disp().closed || r.opened[epd.SSD1675] == nil {
		t.Fatalf("not reopened: %v", r.order)
	}
}

func TestRunStopsWithContext(t *testing.T) {
	r := newRig(t, true)
	c, cancel := context.WithCancel(ctx)
	btn := make(chan Button, 1)
	done := make(chan error, 1)
	go func() { done <- r.p.Run(c, btn) }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
}

func TestNoButtonsMeansNoWizard(t *testing.T) {
	r := newRig(t, true)
	r.p.cfg.NoButtons = true
	r.app.view.Settings.Controller, r.app.view.Settings.DetectPending = "", true
	r.run(5 * time.Minute)
	if len(r.order) != 0 {
		t.Fatalf("wizard ran with no buttons to answer it: %v", r.order)
	}
}
