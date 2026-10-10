package web

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/log"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

// The browser tests drive the real pages in headless Chrome against the
// whole server on the fake graywolf. They need Chrome, so they're
// opt-in: CB_E2E=1 go test ./internal/web -run E2E
const e2eTimeout = 60 * time.Second

// browser is a headless Chrome tab that records page errors: uncaught
// exceptions, console.error, and browser log errors (CSP violations,
// failed loads).
type browser struct {
	t    *testing.T
	ctx  context.Context
	base string
	mu   sync.Mutex
	errs []string
}

func newBrowser(t *testing.T, base string) *browser {
	t.Helper()
	if os.Getenv("CB_E2E") == "" {
		t.Skip("set CB_E2E=1 to run browser tests (needs Chrome)")
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.WindowSize(420, 900))
	actx, cancelA := chromedp.NewExecAllocator(context.Background(), opts...)
	ctx, cancelC := chromedp.NewContext(actx)
	ctx, cancelT := context.WithTimeout(ctx, e2eTimeout)
	t.Cleanup(func() { cancelT(); cancelC(); cancelA() })
	b := &browser{t: t, ctx: ctx, base: base}
	chromedp.ListenTarget(ctx, func(ev any) {
		switch ev := ev.(type) {
		case *runtime.EventExceptionThrown:
			b.record("exception: " + ev.ExceptionDetails.Error())
		case *runtime.EventConsoleAPICalled:
			if ev.Type == runtime.APITypeError {
				b.record(fmt.Sprintf("console.error: %v", ev.Args))
			}
		case *page.EventJavascriptDialogOpening:
			// Accept confirm() prompts; the tests take the confirmed path.
			go func() { _ = chromedp.Run(ctx, page.HandleJavaScriptDialog(true)) }()
		case *log.EventEntryAdded:
			if ev.Entry.Level == log.LevelError && !strings.Contains(ev.Entry.Text, "status of 401") {
				b.record("log: " + ev.Entry.Text + " " + ev.Entry.URL)
			}
		}
	})
	if err := chromedp.Run(ctx, log.Enable()); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *browser) record(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.errs = append(b.errs, s)
}

func (b *browser) run(actions ...chromedp.Action) {
	b.t.Helper()
	if err := chromedp.Run(b.ctx, actions...); err != nil {
		b.t.Fatal(err)
	}
}

// login sets the session cookie directly (the login form has its own test).
func (b *browser) login(token string) {
	b.t.Helper()
	b.run(chromedp.ActionFunc(func(ctx context.Context) error {
		return network.SetCookie(sessionCookie, token).WithURL(b.base).WithHTTPOnly(true).Do(ctx)
	}))
}

// waitText waits until sel's text contains want.
func (b *browser) waitText(sel, want string) {
	b.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		_ = chromedp.Run(b.ctx, chromedp.Text(sel, &got, chromedp.ByQuery, chromedp.NodeVisible))
		if strings.Contains(got, want) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	b.t.Fatalf("%s: want text %q, have %q", sel, want, got)
}

func (b *browser) noErrors() {
	b.t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.errs) > 0 {
		b.t.Fatalf("page errors:\n%s", strings.Join(b.errs, "\n"))
	}
}

func TestE2ELoginFormToKeypad(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceActive))
	b := newBrowser(t, e.srv.URL)
	b.run(chromedp.Navigate(e.srv.URL+"/"),
		chromedp.WaitVisible("#login-form", chromedp.ByQuery),
		chromedp.Click(`input[name="role"][value="volunteer"]`, chromedp.ByQuery),
		chromedp.SendKeys("#login-pw", volPW, chromedp.ByQuery),
		chromedp.Click(`#login-form button[type="submit"]`, chromedp.ByQuery),
		chromedp.WaitVisible("#keys", chromedp.ByQuery))
	b.noErrors()
}

func TestE2EKeypadLogsAndVoids(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceActive))
	b := newBrowser(t, e.srv.URL)
	b.login(e.volunt)
	b.run(chromedp.Navigate(e.srv.URL+"/keypad.html"),
		chromedp.WaitEnabled(`#keys button[aria-label="1"]`, chromedp.ByQuery))
	for _, k := range "142" {
		b.run(chromedp.Click(fmt.Sprintf(`#keys button[aria-label="%c"]`, k), chromedp.ByQuery))
	}
	b.waitText("#display", "142")
	b.run(chromedp.Click("#keys .log", chromedp.ByQuery))
	b.waitText("#log-result", "142 logged")
	b.waitText("#entries", "142")
	b.run(chromedp.Click(`#entries button.danger`, chromedp.ByQuery))
	// A double tap on LOG saves once.
	for _, k := range "143" {
		b.run(chromedp.Click(fmt.Sprintf(`#keys button[aria-label="%c"]`, k), chromedp.ByQuery))
	}
	b.run(chromedp.Evaluate(`(() => { const l = document.querySelector("#keys .log"); l.click(); l.click(); })()`, nil))
	b.waitText("#log-result", "143 logged")
	b.waitText("#entries", "143")
	all, _ := e.st.ListLocal(ctx, 10)
	n143 := 0
	for _, en := range all {
		if en.Bib == 143 {
			n143++
		}
	}
	if n143 != 1 {
		t.Fatalf("after double tap: bib 143 logged %d times, want 1", n143)
	}
	b.run(chromedp.Click(`//ul[@id="entries"]/li[span[1][text()="143"]]/button`, chromedp.BySearch))
	time.Sleep(300 * time.Millisecond)

	// A still-queued entry is removed outright when voided (spec 4.1).
	var left int
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		b.run(chromedp.Evaluate(`document.querySelectorAll("#entries li").length`, &left))
		if left == 0 {
			break
		}
	}
	entries, err := e.st.ListLocal(ctx, 10)
	if err != nil || left != 0 || len(entries) != 0 {
		t.Fatalf("after void: %d shown, store %+v, %v", left, entries, err)
	}
	b.noErrors()
}

func TestE2EAdminTabsCheckpoint(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceActive))
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL + "/admin.html"))
	b.waitText("#tab-race", "Checkpoint open")
	b.waitText("#tabs", "Outbox")
	for _, tab := range []struct{ name, sel, want string }{
		{"Station", "#tab-station", "Station guide"},
		{"Outbox", "#tab-outbox", "Export for HQ"},
		{"Passwords", "#tab-passwords", "Volunteer password"},
		{"Race", "#tab-race", "Reset this node"},
	} {
		b.run(chromedp.Click(fmt.Sprintf(`//nav[@id="tabs"]/button[text()=%q]`, tab.name), chromedp.BySearch))
		b.waitText(tab.sel, tab.want)
	}
	var hqTabs []*cdp.Node
	b.run(chromedp.Nodes(`//nav[@id="tabs"]/button[text()="HQ"]`, &hqTabs, chromedp.BySearch, chromedp.AtLeast(0)))
	if len(hqTabs) != 0 {
		t.Fatal("HQ tab shown on a checkpoint")
	}

	// The guide opens from the Station tab, behind the admin login.
	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="Station"]`, chromedp.BySearch),
		chromedp.Click("#guide-link", chromedp.ByQuery))
	b.waitText("main.guide", "2 m band plan")
	b.run(chromedp.Navigate(e.srv.URL + "/admin.html"))
	b.waitText("#tab-race", "Checkpoint open")

	// Save each settings form; neither may undo the other.
	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="Station"]`, chromedp.BySearch),
		chromedp.WaitVisible("#station_tactical", chromedp.ByQuery),
		chromedp.SetValue("#station_tactical", "Ridge Aid #9", chromedp.ByQuery),
		chromedp.Click(`#race-settings button[type="submit"]`, chromedp.ByQuery))
	b.waitText("#banner", "Race settings saved")
	var hqCodesHidden bool
	b.run(chromedp.Evaluate(`document.querySelector("#hq_local_codes").offsetParent === null`, &hqCodesHidden))
	if !hqCodesHidden {
		t.Error("HQ local codes shown on a checkpoint")
	}
	b.run(chromedp.WaitVisible("#heartbeat_sec", chromedp.ByQuery),
		chromedp.SetValue("#heartbeat_sec", "600", chromedp.ByQuery),
		chromedp.Click(`#messaging-settings button[type="submit"]`, chromedp.ByQuery))
	b.waitText("#banner", "Messaging settings saved")
	cfg, err := e.st.GetSettings(ctx)
	if err != nil || cfg.StationTactical != "Ridge Aid #9" || cfg.HeartbeatSec != 600 {
		t.Fatalf("settings = %q, %d, %v", cfg.StationTactical, cfg.HeartbeatSec, err)
	}
	b.noErrors()
}

func TestE2EAdminHQAndBoard(t *testing.T) {
	e := newEnv(t, hqSettings(store.RaceActive))
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL + "/admin.html"))
	b.waitText("#tabs", "Branding")

	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="HQ"]`, chromedp.BySearch))
	b.waitText("#tab-hq", "Checkpoint health")
	b.run(chromedp.SendKeys(`#tab-hq input[placeholder="Code"]`, "AS5", chromedp.ByQuery),
		chromedp.SendKeys(`#tab-hq input[placeholder="Name"]`, "Ridge Aid", chromedp.ByQuery),
		chromedp.Click(`//section[@id="tab-hq"]//button[text()="Add"]`, chromedp.BySearch))
	b.waitText("#tab-hq", "Ridge Aid")

	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="Branding"]`, chromedp.BySearch),
		chromedp.WaitVisible("#b-header", chromedp.ByQuery))
	b.waitText("#b-contrast", ":1")
	b.run(chromedp.SendKeys("#b-header", "Ridge 50K Live", chromedp.ByQuery),
		chromedp.Click(`//section[@id="tab-branding"]//button[text()="Save"]`, chromedp.BySearch))
	b.waitText("#banner", "Branding saved")

	b.run(chromedp.Navigate(e.srv.URL + "/board.html"))
	b.waitText("body", "Ridge 50K Live")
	b.waitText("body", "Ridge Aid")
	b.noErrors()
}

func TestE2ELinkCheckTab(t *testing.T) {
	e := newEnv(t, hqSettings(store.RaceSetup))
	if err := e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Ridge", CourseOrder: 1, ExpectedCall: "N0CALL-1"}); err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL+"/admin.html"),
		chromedp.Click(`//nav[@id="tabs"]/button[text()="Link check"]`, chromedp.BySearch),
		chromedp.WaitVisible("#lc-to", chromedp.ByQuery))
	// HQ's checkpoints are listed by callsign, plus "Other callsign…".
	b.waitText("#lc-to", "AS5 Ridge (N0CALL-1)")
	b.waitText("#lc-to", "Other callsign")
	b.run(chromedp.SetValue("#lc-to", "N0CALL-1", chromedp.ByQuery),
		chromedp.Click(`//section[@id="tab-link"]//button[text()="Run link check"]`, chromedp.BySearch))
	b.waitText("#banner", "Link check started")
	// No service ticks in this test server, so the run waits to start.
	b.waitText("#tab-link", "Starting…")
	b.run(chromedp.Click(`//section[@id="tab-link"]//button[text()="Cancel"]`, chromedp.BySearch))
	b.waitText("#tab-link", "Cancelled")
	c, _ := e.st.GetLinkCheck(ctx, 1)
	if c.PeerCall != "N0CALL-1" || c.State != store.LinkCheckCancelled {
		t.Fatalf("check = %+v", c)
	}
	// The HQ health panel shows the link column.
	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="HQ"]`, chromedp.BySearch))
	b.waitText("#tab-hq", "never checked")
	b.noErrors()
}

// The bib reaches the node and is saved, but the reply is lost (Wi-Fi
// drops on the way back). The keypad keeps the digits and the request
// id; tapping LOG again must not log the bib twice.
func TestE2EKeypadSurvivesLostReply(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceActive))
	b := newBrowser(t, e.srv.URL)
	var drop atomic.Bool
	drop.Store(true)
	chromedp.ListenTarget(b.ctx, func(ev any) {
		if ev, ok := ev.(*fetch.EventRequestPaused); ok {
			go func() {
				c := chromedp.FromContext(b.ctx)
				ectx := cdp.WithExecutor(b.ctx, c.Target)
				if ev.Request.Method == "POST" && drop.CompareAndSwap(true, false) {
					_ = fetch.FailRequest(ev.RequestID, network.ErrorReasonInternetDisconnected).Do(ectx)
					return
				}
				_ = fetch.ContinueRequest(ev.RequestID).Do(ectx)
			}()
		}
	})
	b.login(e.volunt)
	b.run(chromedp.Navigate(e.srv.URL+"/keypad.html"),
		chromedp.WaitEnabled(`#keys button[aria-label="7"]`, chromedp.ByQuery),
		fetch.Enable().WithPatterns([]*fetch.RequestPattern{{URLPattern: "*/api/entries", RequestStage: fetch.RequestStageResponse}}),
		chromedp.Click(`#keys button[aria-label="7"]`, chromedp.ByQuery),
		chromedp.Click(`#keys button[aria-label="7"]`, chromedp.ByQuery),
		chromedp.Click("#keys .log", chromedp.ByQuery))
	b.waitText("#log-result", "NOT confirmed yet")
	b.waitText("#display", "77") // the digits stay for the retry
	if all, _ := e.st.ListLocal(ctx, 10); len(all) != 1 {
		t.Fatalf("the node didn't save the first attempt: %d entries", len(all))
	}
	b.run(chromedp.Click("#keys .log", chromedp.ByQuery))
	b.waitText("#log-result", "77 logged")
	if all, _ := e.st.ListLocal(ctx, 10); len(all) != 1 {
		t.Fatalf("entries after retry = %d, want 1", len(all))
	}
	b.mu.Lock()
	b.errs = nil // the dropped reply logs a load failure, as it should
	b.mu.Unlock()
}

func TestE2EClockBannerSetsTime(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) { d.Clock = raceclock.NewClock(nil, nil) })
	b := newBrowser(t, e.srv.URL)
	b.login(e.volunt)
	b.run(chromedp.Navigate(e.srv.URL + "/keypad.html"))
	b.waitText("#clock-banner", "Clock not set")
	b.run(chromedp.Click(`#clock-banner button`, chromedp.ByQuery))
	b.run(chromedp.WaitNotVisible("#clock-banner", chromedp.ByQuery))
	b.noErrors()
}

func TestE2EAdminOpensCheckpoint(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL + "/admin.html"))
	b.waitText("#tab-race", "Checkpoint not open")
	// confirm() (with the link-check warning) is accepted by the harness.
	b.run(chromedp.Click(`//section[@id="tab-race"]//button[text()="Open checkpoint"]`, chromedp.BySearch))
	b.waitText("#state", "Checkpoint open")
	cfg, _ := e.st.GetSettings(ctx)
	if cfg.RaceState != store.RaceActive {
		t.Fatalf("state = %s", cfg.RaceState)
	}
	b.noErrors()
}

func TestE2EPanelTab(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL+"/admin.html"),
		chromedp.Click(`//nav[@id="tabs"]/button[text()="Panel"]`, chromedp.BySearch),
		chromedp.WaitVisible(`#tab-panel img.panel-preview`, chromedp.ByQuery))
	// The preview is a real rendering of the status screen.
	var width int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && width == 0; time.Sleep(100 * time.Millisecond) {
		b.run(chromedp.Evaluate(`document.querySelector("#tab-panel img.panel-preview").naturalWidth`, &width))
	}
	if width != 250 {
		t.Fatalf("preview width = %d", width)
	}
	// The menu editor is shelved with the buttons.
	var editors []*cdp.Node
	b.run(chromedp.Nodes(`#tab-panel tbody input[aria-label="Label"]`, &editors, chromedp.ByQuery, chromedp.AtLeast(0)))
	if len(editors) != 0 {
		t.Fatal("menu editor still shown")
	}
	// Settings save.
	b.run(chromedp.SetValue("#pn-refresh", "10", chromedp.ByQuery),
		chromedp.Click(`//section[@id="tab-panel"]//button[text()="Save"]`, chromedp.BySearch))
	b.waitText("#banner", "Panel settings saved")
	if ps, _ := e.st.GetPanelSettings(ctx); ps.RefreshMin != 10 {
		t.Fatalf("refresh = %d", ps.RefreshMin)
	}
	b.noErrors()
}

// The radio check: a summary before Start, the checklist on request.
func TestE2ERadioCheck(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	b := newBrowser(t, e.srv.URL)
	b.login(e.admin)
	b.run(chromedp.Navigate(e.srv.URL + "/admin.html"))
	// No host monitor in tests: the modem's keep-up is honestly unchecked.
	b.waitText("#radio-summary", "Radio: no problems found; Modem keeping up not checked yet")
	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="Station"]`, chromedp.BySearch),
		chromedp.Click("#radio-check", chromedp.ByQuery))
	b.waitText("#radio-result", "Push-to-talk")
	b.waitText("#radio-result", "Modem keeping up")
	// A broken setup shows up in the summary.
	e.gw.EditRadio(func(r *gwfake.RadioSetup) { r.Channels[0].PTT.Configured = false })
	b.run(chromedp.Click(`//nav[@id="tabs"]/button[text()="Race"]`, chromedp.BySearch))
	b.waitText("#radio-summary", "Push-to-talk failed")
	b.noErrors()
}
