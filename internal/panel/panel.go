package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"log/slog"
	"time"

	"checkin-board/internal/panel/epd"
	"checkin-board/internal/panel/menu"
)

// Button is one of the bonnet's two buttons.
type Button int

// The buttons: top moves to the next item, bottom selects.
const (
	Top Button = iota
	Bottom
)

// Timing and limits (spec 8.4).
const (
	pollEvery  = 30 * time.Second // how often the app is asked for the view
	retryEvery = 5 * time.Second  // after a failed poll
	// floor: Adafruit warns against refreshing an e-ink panel more often
	// than every 3 minutes long-term. Every unattended full refresh
	// (status, app down, test patterns, the wizard's first frame) waits
	// for it, failed attempts included; the app remembers it across
	// panel restarts.
	floor       = 3 * time.Minute
	idleTimeout = 30 * time.Second
	messageFor  = 10 * time.Second
	// While someone uses the buttons: at most one partial refresh a
	// second (one full refresh every 2 s on panels without partial), and
	// at most a budget per floor window, so a stuck or bouncing button
	// can't wear the panel out.
	partialEvery  = time.Second
	fullEvery     = 2 * time.Second
	partialBudget = 60
	fullBudget    = 6
	confirmGuard  = 600 * time.Millisecond // ignore a bounce onto "yes"
	wizardStep    = 20 * time.Second
	wizardSettle  = 3 * time.Second // a press this soon after a switch is ignored
	wizardRounds  = 3               // passes over the controllers before giving up
	patternFor    = 30 * time.Second
	tickEvery     = 250 * time.Millisecond
)

// ErrAppUnreachable: the app didn't answer the panel.
var ErrAppUnreachable = errors.New("panel: the app didn't answer")

// Source is the app, reached over the local hook.
type Source interface {
	View(ctx context.Context) (View, error)
	Act(ctx context.Context, id int64, req ActionRequest) (ActionResult, error)
	Refreshed(ctx context.Context, at time.Time) error
	SetController(ctx context.Context, controller string) error
	DetectGaveUp(ctx context.Context) error
}

// Opener opens the display with a given controller.
type Opener func(controller string) (epd.Display, error)

// Config wires a Panel.
type Config struct {
	Source Source
	Open   Opener
	// Addrs lists the node's network addresses for the status screen.
	Addrs func() []string
	Now   func() time.Time
	Log   *slog.Logger
}

type mode int

const (
	modeStatus mode = iota
	modeMenu
	modeTargets
	modeConfirm
	modeMessage
	modeWizard
	modePattern
)

// entry is one line of a menu.
type entry struct {
	label  string
	item   *MenuItem
	target *Target
	back   bool
}

type drawn int

const (
	drewNothing drawn = iota // waiting for the rate limit, or over budget
	drewPartial
	drewFull
)

// Panel is the panel's state machine. Tick and Press must be called from
// one goroutine (Run does).
type Panel struct {
	cfg Config
	log *slog.Logger

	view     *View
	viewErr  error
	nextPoll time.Time
	boot     int64
	seenInit bool
	seenRef  int64
	seenTest int64
	addrs    []string
	key, imp string // what the status screen would show now

	disp           epd.Display
	dispController string

	lastFull    time.Time
	lastPartial time.Time
	lastIntFull time.Time
	partials    []time.Time // interactive refreshes in the current floor window
	fulls       []time.Time
	pending     *image.Gray // an interactive frame waiting for the rate limit
	pendingFull bool        // a full refresh is owed (ghosting after the menu)
	drawnKey    string
	drawnImp    string
	pattern     string // a test pattern waiting for the floor

	mode      mode
	entries   []entry
	cursor    int
	title     string
	menuPos   int
	picked    *MenuItem
	confirm   entry
	confirmAt time.Time
	message   []string
	until     time.Time
	lastInput time.Time

	wizardDraws  int
	wizardNext   time.Time
	wizardSwitch time.Time
}

// New returns a panel.
func New(cfg Config) *Panel {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Addrs == nil {
		cfg.Addrs = func() []string { return nil }
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Panel{cfg: cfg, log: log}
}

// Run drives the panel until ctx ends.
func (p *Panel) Run(ctx context.Context, buttons <-chan Button) error {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	defer p.closeDisplay()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case b := <-buttons:
			p.Press(ctx, b)
		case <-t.C:
			p.Tick(ctx)
		}
	}
}

func (p *Panel) enabled() bool { return p.view != nil && p.view.Settings.Enabled }

// Tick polls the app when due and advances timers.
func (p *Panel) Tick(ctx context.Context) {
	now := p.cfg.Now()
	if !now.Before(p.nextPoll) {
		p.poll(ctx, now)
	}
	if !p.enabled() {
		return
	}
	if p.pending != nil {
		p.interactive(ctx, p.pending, now)
	}
	if p.pattern != "" && p.mode == modeStatus && p.canFull(now) {
		p.showPattern(ctx, p.pattern, now)
	}
	switch p.mode {
	case modeWizard:
		p.wizardTick(ctx, now)
	case modeMenu, modeTargets, modeConfirm:
		if now.Sub(p.lastInput) >= idleTimeout {
			p.toStatus(ctx, now)
		}
	case modeMessage:
		if !now.Before(p.until) {
			p.toStatus(ctx, now)
		}
	case modePattern:
		if !now.Before(p.until) {
			// Back to status at the next allowed full refresh.
			p.mode, p.pendingFull = modeStatus, true
		}
	case modeStatus:
		p.statusTick(ctx, now)
	}
}

func (p *Panel) poll(ctx context.Context, now time.Time) {
	p.addrs = p.cfg.Addrs()
	v, err := p.cfg.Source.View(ctx)
	if err != nil {
		p.viewErr, p.nextPoll = err, now.Add(retryEvery)
		p.key, p.imp = "down:"+err.Error(), "down"
		return
	}
	p.viewErr, p.nextPoll, p.view = nil, now.Add(pollEvery), &v
	if !p.seenInit || v.Boot != p.boot {
		// First view, or the app restarted: its request counters started
		// over, so take them as seen rather than replay old requests.
		p.seenInit, p.boot, p.seenRef, p.seenTest = true, v.Boot, v.RefreshSeq, v.TestPatternSeq
	}
	p.key, p.imp = p.statusKey()
	if !v.Settings.Enabled {
		p.mode, p.pending = modeStatus, nil
		return
	}
	switch c := v.Settings.Controller; {
	case c == "":
		if v.Settings.DetectPending && p.mode != modeWizard {
			p.mode, p.wizardDraws, p.wizardNext = modeWizard, 0, now
		}
	default:
		if p.mode == modeWizard {
			p.mode = modeStatus
		}
		p.ensureDisplay(c)
	}
	if v.TestPatternSeq != p.seenTest {
		p.seenTest = v.TestPatternSeq
		if epd.Known(v.TestPattern) {
			p.pattern = v.TestPattern // drawn when the floor allows
		}
	}
}

func (p *Panel) ensureDisplay(c string) {
	if p.disp != nil && p.dispController == c {
		return
	}
	p.closeDisplay()
	d, err := p.cfg.Open(c)
	if err != nil {
		p.log.Warn("panel: open display", "controller", c, "err", err)
		return
	}
	p.disp, p.dispController, p.drawnKey = d, c, ""
}

func (p *Panel) closeDisplay() {
	if p.disp != nil {
		if err := p.disp.Close(); err != nil {
			p.log.Warn("panel: close display", "err", err)
		}
	}
	p.disp, p.dispController = nil, ""
}

func (p *Panel) rotation() int {
	if p.view == nil {
		return 0
	}
	return p.view.Settings.Rotation
}

// allowedAt is the earliest next unattended full refresh.
func (p *Panel) allowedAt() time.Time {
	var at time.Time
	if !p.lastFull.IsZero() {
		at = p.lastFull.Add(floor)
	}
	if p.view != nil && p.view.FullAllowedAt != nil && p.view.FullAllowedAt.After(at) {
		at = *p.view.FullAllowedAt
	}
	return at
}

func (p *Panel) canFull(now time.Time) bool { return !now.Before(p.allowedAt()) }

// full refreshes the whole panel. Every attempt counts against the
// floor, failed ones too, so a failing panel isn't retried 4 times a
// second.
func (p *Panel) full(ctx context.Context, d epd.Display, img *image.Gray, now time.Time) bool {
	if d == nil {
		return false
	}
	p.lastFull = now
	if err := p.cfg.Source.Refreshed(ctx, now); err != nil {
		p.log.Warn("panel: report refresh to the app", "err", err)
	}
	if err := d.Full(Rotate(img, p.rotation())); err != nil {
		p.log.Warn("panel: full refresh", "err", err)
		return false
	}
	p.pending = nil
	return true
}

// withinBudget prunes the window and reports whether one more
// interactive refresh fits.
func withinBudget(log *[]time.Time, budget int, now time.Time) bool {
	keep := (*log)[:0]
	for _, t := range *log {
		if now.Sub(t) < floor {
			keep = append(keep, t)
		}
	}
	*log = keep
	return len(keep) < budget
}

// interactive draws a frame while someone is using the buttons: a
// partial refresh where the panel has one, else a full one, rate-limited
// and budgeted (a later frame replaces a waiting one).
func (p *Panel) interactive(ctx context.Context, img *image.Gray, now time.Time) drawn {
	if p.disp == nil {
		return drewNothing
	}
	if p.disp.CanPartial() {
		if now.Sub(p.lastPartial) < partialEvery {
			p.pending = img
			return drewNothing
		}
		p.pending = nil
		if !withinBudget(&p.partials, partialBudget, now) {
			return drewNothing
		}
		p.lastPartial = now
		p.partials = append(p.partials, now)
		if err := p.disp.Partial(Rotate(img, p.rotation())); err != nil {
			p.log.Warn("panel: partial refresh", "err", err)
		}
		return drewPartial
	}
	if now.Sub(p.lastIntFull) < fullEvery {
		p.pending = img
		return drewNothing
	}
	p.pending = nil
	if !withinBudget(&p.fulls, fullBudget, now) {
		return drewNothing
	}
	p.lastIntFull = now
	p.fulls = append(p.fulls, now)
	if p.full(ctx, p.disp, img, now) {
		return drewFull
	}
	return drewNothing
}

// statusKey identifies what the status screen shows, minus the clock;
// important is the part that changes it early.
func (p *Panel) statusKey() (key, important string) {
	st := p.view.Status
	st.Now = time.Time{}
	b, _ := json.Marshal(struct {
		S     Status
		Addrs []string
		Rot   int
	}{st, p.addrs, p.view.Settings.Rotation})
	return string(b), fmt.Sprint(st.Role, st.RaceState, st.GraywolfOK, len(st.Warnings))
}

func (p *Panel) statusScreen(now time.Time) *image.Gray {
	if p.viewErr != nil {
		return AppDownScreen(shortErr(p.viewErr), now)
	}
	return StatusScreen(*p.view, p.addrs)
}

func (p *Panel) statusTick(ctx context.Context, now time.Time) {
	if p.disp == nil || !p.canFull(now) {
		return
	}
	interval := p.view != nil && now.Sub(p.lastFull) >= time.Duration(p.view.Settings.RefreshMin)*time.Minute
	requested := p.viewErr == nil && p.view.RefreshSeq != p.seenRef
	changed := p.key != p.drawnKey
	if p.drawnKey == "" || requested || p.pendingFull || (changed && (interval || p.imp != p.drawnImp)) {
		key, imp := p.key, p.imp
		if p.full(ctx, p.disp, p.statusScreen(now), now) {
			p.drawnKey, p.drawnImp, p.pendingFull = key, imp, false
			if p.view != nil {
				p.seenRef = p.view.RefreshSeq
			}
		}
	}
}

// toStatus leaves the menu: the status screen comes back at once, and,
// after a partial refresh, a full one at the next allowed time clears
// the ghosting.
func (p *Panel) toStatus(ctx context.Context, now time.Time) {
	p.mode = modeStatus
	if p.view == nil {
		return
	}
	switch p.interactive(ctx, p.statusScreen(now), now) {
	case drewFull:
		p.drawnKey, p.drawnImp, p.pendingFull = p.key, p.imp, false
	default:
		p.pendingFull = true
	}
}

// Press handles a button.
func (p *Panel) Press(ctx context.Context, b Button) {
	now := p.cfg.Now()
	p.lastInput = now
	if !p.enabled() {
		return
	}
	switch p.mode {
	case modeStatus:
		p.openMenu(ctx, now, 0)
	case modeWizard:
		if now.Sub(p.wizardSwitch) >= wizardSettle {
			p.pickController(ctx, now)
		}
	case modeMenu, modeTargets:
		if b == Top {
			p.cursor = (p.cursor + 1) % len(p.entries)
			p.drawMenu(ctx, now)
			return
		}
		p.choose(ctx, p.entries[p.cursor], now)
	case modeConfirm:
		if b == Top {
			p.openMenu(ctx, now, p.menuPos)
			return
		}
		if now.Sub(p.confirmAt) >= confirmGuard {
			p.execute(ctx, p.confirm, now)
		}
	case modeMessage, modePattern:
		p.toStatus(ctx, now)
	}
}

func (p *Panel) openMenu(ctx context.Context, now time.Time, cursor int) {
	p.entries = p.entries[:0]
	for i := range p.view.Menu {
		it := &p.view.Menu[i]
		if it.Available && it.Enabled {
			p.entries = append(p.entries, entry{label: it.Label, item: it})
		}
	}
	p.entries = append(p.entries, entry{label: "Back", back: true})
	p.mode, p.title, p.cursor = modeMenu, "Menu", min(cursor, len(p.entries)-1)
	p.drawMenu(ctx, now)
}

func (p *Panel) drawMenu(ctx context.Context, now time.Time) {
	labels := make([]string, len(p.entries))
	for i, e := range p.entries {
		labels[i] = e.label
	}
	p.interactive(ctx, MenuScreen(p.title, labels, p.cursor), now)
}

func (p *Panel) show(ctx context.Context, title string, lines []string, now time.Time) {
	p.mode, p.message, p.until = modeMessage, lines, now.Add(messageFor)
	p.interactive(ctx, MessageScreen(title, lines), now)
}

func (p *Panel) choose(ctx context.Context, e entry, now time.Time) {
	switch {
	case e.back:
		p.toStatus(ctx, now)
	case e.target != nil:
		p.ask(ctx, e, p.picked.Confirm, now)
	default:
		p.menuPos, p.picked = p.cursor, e.item
		p.chooseItem(ctx, e, now)
	}
}

func (p *Panel) chooseItem(ctx context.Context, e entry, now time.Time) {
	st := p.view.Status
	switch e.item.Action {
	case menu.Status:
		p.toStatus(ctx, now)
	case menu.ShowNetwork:
		lines := []string{}
		for _, a := range p.addrs {
			lines = append(lines, fmt.Sprintf("http://%s:%d", a, st.Port))
		}
		if len(lines) == 0 {
			lines = append(lines, "No network address yet.")
		}
		p.show(ctx, "Network", lines, now)
	case menu.ShowLink:
		lines := []string{"No link check yet."}
		if l := st.LastLink; l != nil {
			lines = []string{
				fmt.Sprintf("%s to %s", l.Verdict, l.Peer),
				fmt.Sprintf("Heard there %d/%d, ACKed %d/%d", l.Uplink, l.Count, l.RoundTrip, l.Count),
				fmt.Sprintf("%s ago (%s)", ago(l.At, st.Now), hhmm(l.At)),
			}
		}
		p.show(ctx, "Last link check", lines, now)
	case menu.Refresh:
		if p.canFull(now) {
			p.mode, p.pendingFull = modeStatus, true
			p.statusTick(ctx, now)
			return
		}
		mins := int(p.allowedAt().Sub(now)/time.Minute) + 1
		p.show(ctx, "Refresh", []string{"The display rests 3 minutes", fmt.Sprintf("between refreshes: next in %d min.", mins)}, now)
	case menu.LinkCheck:
		if st.Role != "hq" {
			p.ask(ctx, e, e.item.Confirm, now)
			return
		}
		p.entries = p.entries[:0]
		for i := range p.view.Targets {
			t := &p.view.Targets[i]
			if t.Call != "" {
				p.entries = append(p.entries, entry{label: t.Code + " " + t.Name, target: t})
			}
		}
		if len(p.entries) == 0 {
			p.show(ctx, "Link check", []string{"No checkpoint has a callsign.", "Add them on Admin > HQ."}, now)
			return
		}
		p.entries = append(p.entries, entry{label: "Back", back: true})
		p.mode, p.title, p.cursor = modeTargets, "Link check to", 0
		p.drawMenu(ctx, now)
	default:
		p.ask(ctx, e, e.item.Confirm, now)
	}
}

// ask shows the confirm screen, or runs the item at once.
func (p *Panel) ask(ctx context.Context, e entry, confirm bool, now time.Time) {
	p.confirm = e
	if !confirm {
		p.execute(ctx, e, now)
		return
	}
	label := e.label
	if e.target != nil {
		label = p.picked.Label + ": " + e.target.Code
	}
	p.mode, p.confirmAt = modeConfirm, now
	p.interactive(ctx, ConfirmScreen(label), now)
}

func (p *Panel) execute(ctx context.Context, e entry, now time.Time) {
	item := p.picked
	req := ActionRequest{Action: item.Action, MenuRev: p.view.MenuRev}
	if e.target != nil {
		req.To = e.target.Call
	}
	res, err := p.cfg.Source.Act(ctx, item.ID, req)
	p.nextPoll = now // show the change soon
	if err != nil {
		p.show(ctx, item.Label, []string{"Failed:", shortErr(err)}, now)
		return
	}
	p.show(ctx, item.Label, []string{res.Message}, now)
}

// shortErr fits an error on the panel.
func shortErr(err error) string {
	if errors.Is(err, ErrAppUnreachable) {
		return "The app didn't answer."
	}
	return err.Error()
}

func (p *Panel) wizardTick(ctx context.Context, now time.Time) {
	if now.Before(p.wizardNext) {
		return
	}
	// The wizard's first frame waits for the floor like any unattended
	// refresh (a restarting panel can't loop it); later frames follow
	// every wizardStep while someone at the node looks for a readable one.
	if p.wizardDraws == 0 && !p.canFull(now) {
		return
	}
	cs := epd.Controllers()
	if p.wizardDraws >= wizardRounds*len(cs) {
		p.closeDisplay()
		p.mode = modeStatus
		if err := p.cfg.Source.DetectGaveUp(ctx); err != nil {
			p.log.Warn("panel: report detection gave up", "err", err)
		}
		if p.view != nil {
			p.view.Settings.DetectPending = false
		}
		p.log.Warn("panel: no controller confirmed; choose one or detect again on Admin > Panel")
		return
	}
	i := p.wizardDraws % len(cs)
	c := cs[i]
	p.wizardDraws++
	p.wizardNext, p.wizardSwitch = now.Add(wizardStep), now
	p.closeDisplay()
	d, err := p.cfg.Open(c)
	if err != nil {
		p.log.Warn("panel: open display", "controller", c, "err", err)
		return
	}
	p.disp, p.dispController = d, c
	p.full(ctx, d, WizardScreen(c, i, len(cs)), now)
}

func (p *Panel) pickController(ctx context.Context, now time.Time) {
	c := p.dispController
	if c == "" {
		return
	}
	if err := p.cfg.Source.SetController(ctx, c); err != nil {
		p.log.Warn("panel: save controller", "err", err)
		return
	}
	p.view.Settings.Controller, p.view.Settings.DetectPending = c, false
	p.mode = modeStatus
	p.key, p.imp = p.statusKey()
	// Someone is at the node: show the status now.
	if p.full(ctx, p.disp, p.statusScreen(now), now) {
		p.drawnKey, p.drawnImp = p.key, p.imp
	}
}

// showPattern draws a test pattern with controller c (when the floor
// allows). The main display is closed first: one handle on the hardware
// at a time.
func (p *Panel) showPattern(ctx context.Context, c string, now time.Time) {
	p.pattern = ""
	if c != p.dispController {
		p.closeDisplay()
	}
	d := p.disp
	if d == nil {
		var err error
		if d, err = p.cfg.Open(c); err != nil {
			p.log.Warn("panel: open display for test pattern", "controller", c, "err", err)
			return
		}
		defer func() {
			if err := d.Close(); err != nil {
				p.log.Warn("panel: close display", "err", err)
			}
		}()
	}
	p.full(ctx, d, TestPattern(c), now)
	p.mode, p.until, p.pending = modePattern, now.Add(patternFor), nil
}
