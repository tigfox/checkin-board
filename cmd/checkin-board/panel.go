package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"checkin-board/internal/config"
	"checkin-board/internal/panel"
	"checkin-board/internal/panel/epd"
)

// runPanel runs the node panel (spec 8.4) as its own process: it reads
// the app's address and hook token from the app's environment, and
// drives the e-ink display and buttons. Until the hardware drivers land
// (phase 14), -display png:DIR writes each frame to DIR as a PNG and
// -buttons stdin reads "t" (top) and "b" (bottom) lines, for trying the
// panel on any machine.
func runPanel(ctx context.Context, env config.Env, args []string, stdin io.Reader, log *slog.Logger) error {
	fs := flag.NewFlagSet("panel", flag.ContinueOnError)
	display := fs.String("display", "epd", `"epd" (the bonnet) or "png:DIR" (write frames to DIR)`)
	buttons := fs.String("buttons", "stdin", `"gpio" (the bonnet), "stdin" (t/b lines) or "none"`)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.PanelFrom(env)
	if err != nil {
		return err
	}
	open, err := displayOpener(*display)
	if err != nil {
		return err
	}
	btn := make(chan panel.Button, 8)
	switch *buttons {
	case "stdin":
		go readButtons(stdin, btn)
	case "none":
	default:
		return fmt.Errorf("panel: buttons %q: the bonnet's GPIO buttons arrive with its driver (phase 14); use stdin or none", *buttons)
	}
	log.Info("panel starting", "app", cfg.AppURL, "display", *display, "buttons", *buttons)
	p := panel.New(panel.Config{Source: panel.NewClient(cfg.AppURL, cfg.Token), Open: open, Addrs: panel.NodeAddrs, Log: log})
	return p.Run(ctx, btn)
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
		return nil, errors.New("panel: the e-ink driver arrives in phase 14; use -display png:DIR to try the panel")
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
