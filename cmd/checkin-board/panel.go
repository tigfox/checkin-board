package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"checkin-board/internal/config"
	"checkin-board/internal/panel"
	"checkin-board/internal/panel/epd"
	"checkin-board/internal/panel/epd/bonnet"
)

// runPanel runs the node panel (spec 8.4) as its own process: it reads
// the app's address and hook token from the app's environment, and
// drives the e-ink bonnet's display and buttons. For trying it on any
// machine, -display png:DIR writes each frame to DIR as a PNG and
// -buttons stdin reads "t" (top) and "b" (bottom) lines. -test
// CONTROLLER draws a test pattern and a partial update on the bonnet
// and reports how long each took, without the app.
func runPanel(ctx context.Context, env config.Env, args []string, stdin io.Reader, log *slog.Logger) error {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	display := fs.String("display", "epd", `"epd" (the bonnet) or "png:DIR" (write frames to DIR)`)
	// The buttons and menu are shelved (no partial refresh works on the
	// bonnet, and a full refresh per press is too slow and too much wear),
	// so the panel shows status only unless buttons are asked for.
	buttons := fs.String("buttons", "none", `"none" (status only), "gpio" (the bonnet, edge events), "poll" (polled), or "stdin" (t/b lines)`)
	test := fs.String("test", "", "draw a test pattern with this controller ("+strings.Join(epd.Controllers(), ", ")+") and exit")
	testPartial := fs.String("test-partial", "", "bench: try each partial refresh variant with this controller, timing each, and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	open, err := displayOpener(*display)
	if err != nil {
		return err
	}
	if *test != "" {
		return testDisplay(open, *test, log)
	}
	if *testPartial != "" {
		return testPartials(open, *testPartial, log, time.Sleep)
	}
	cfg, err := config.PanelFrom(env)
	if err != nil {
		return err
	}
	btn := make(chan panel.Button, 8)
	switch *buttons {
	case "stdin":
		go readButtons(stdin, btn)
	case "gpio", "poll":
		read := bonnet.Buttons
		if *buttons == "poll" {
			read = bonnet.PollButtons
		}
		go func() {
			raw := make(chan epd.Button, 8)
			go func() {
				if err := read(ctx, raw); err != nil && ctx.Err() == nil {
					log.Error("panel: buttons", "err", err)
				}
			}()
			for {
				select {
				case <-ctx.Done():
					return
				case b := <-raw:
					// Logged so a press can be confirmed from the journal.
					log.Info("panel: button", "button", map[epd.Button]string{epd.Top: "top", epd.Bottom: "bottom"}[b])
					btn <- panel.Button(b)
				}
			}
		}()
	case "none":
	default:
		return fmt.Errorf("panel: unknown buttons %q", *buttons)
	}
	log.Info("panel starting", "app", cfg.AppURL, "display", *display, "buttons", *buttons)
	p := panel.New(panel.Config{Source: panel.NewClient(cfg.AppURL, cfg.Token), Open: open, Addrs: panel.NodeAddrs, Log: log,
		NoButtons: *buttons == "none"})
	return p.Run(ctx, btn)
}

// testDisplay draws a test pattern (full refresh) and then a menu
// (partial refresh, where the controller has one), timing each: a
// refresh that returns at once or times out points at the wrong
// controller or a loose bonnet.
func testDisplay(open panel.Opener, controller string, log *slog.Logger) error {
	if !epd.Known(controller) {
		return fmt.Errorf("panel: unknown controller %q (one of %s)", controller, strings.Join(epd.Controllers(), ", "))
	}
	d, err := open(controller)
	if err != nil {
		return err
	}
	defer d.Close()
	start := time.Now()
	if err := d.Full(panel.TestPattern(controller)); err != nil {
		return err
	}
	log.Info("panel test: full refresh", "controller", controller, "took", time.Since(start).Round(10*time.Millisecond))
	if !d.CanPartial() {
		return nil
	}
	start = time.Now()
	if err := d.Partial(panel.MenuScreen("Partial refresh test", []string{"If this replaced the", "test pattern cleanly,", "partial refresh works."}, 0)); err != nil {
		return err
	}
	log.Info("panel test: partial refresh", "controller", controller, "took", time.Since(start).Round(10*time.Millisecond))
	return nil
}

// testPartials tries each partial refresh variant: a full refresh to
// "Next: test N", then variant N draws "Test N WORKED". Someone at the
// panel reports which N they saw; the log has each update's busy time.
func testPartials(open panel.Opener, controller string, log *slog.Logger, sleep func(time.Duration)) error {
	if !epd.Known(controller) {
		return fmt.Errorf("panel: unknown controller %q", controller)
	}
	d, err := open(controller)
	if err != nil {
		return err
	}
	defer d.Close()
	for v := 1; ; v++ {
		if !epd.SetPartialVariant(d, v) {
			break
		}
		if err := d.Full(panel.MessageScreen("Partial refresh test", []string{fmt.Sprintf("Next: test %d.", v), "Watch for TEST WORKED."})); err != nil {
			return err
		}
		full := epd.UpdateTook(d)
		sleep(3 * time.Second)
		if err := d.Partial(panel.WizardLikeScreen(fmt.Sprintf("TEST %d", v), "WORKED")); err != nil {
			return err
		}
		log.Info("panel partial test", "variant", v, "full_busy", full.Round(time.Millisecond), "partial_busy", epd.UpdateTook(d).Round(time.Millisecond))
		sleep(5 * time.Second)
	}
	return d.Full(panel.MessageScreen("Partial refresh test", []string{"Done: tell us which", "TEST n WORKED you saw."}))
}

// displayOpener picks the display backend.
func displayOpener(spec string) (panel.Opener, error) {
	switch {
	case strings.HasPrefix(spec, "png:") && len(spec) > len("png:"):
		dir := strings.TrimPrefix(spec, "png:")
		return func(c string) (epd.Display, error) {
			// SSD1675 bonnets have no partial refresh; act like them.
			return panel.NewPNGDisplay(dir, c, c != epd.SSD1675), nil
		}, nil
	case spec == "epd":
		return bonnet.Open, nil
	}
	return nil, fmt.Errorf("panel: unknown display %q", spec)
}

func parseButton(s string) (panel.Button, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "t", "top", "n", "next":
		return panel.Top, true
	case "b", "bottom", "s", "select":
		return panel.Bottom, true
	}
	return 0, false
}

func readButtons(r io.Reader, out chan<- panel.Button) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		if b, ok := parseButton(sc.Text()); ok {
			out <- b
		}
	}
}
