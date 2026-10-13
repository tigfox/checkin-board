package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"checkin-board/internal/app"
	"checkin-board/internal/cablewatch"
	"checkin-board/internal/config"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/hostmon"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/radiocheck"
	"checkin-board/internal/radiokind"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
	"checkin-board/internal/web"
)

// messaging is how this node sends race traffic: graywolf, or a UV-K5
// radio (phase 17), with what the app and the web pages need of it.
type messaging struct {
	kind radiokind.Kind
	gw   interface {
		app.Graywolf
		web.Graywolf
	}
	hold  func() string    // why reports must wait ("": they needn't)
	cable web.CableStatus  // graywolf: the AIOC watcher
	radio radiocheck.Radio // radio node: the backend
	local bool             // graywolf runs on this machine
	tools *radioTools      // UV-K5 node: UV Studio's bridge, the flasher
}

// chooseRadio is the radio kind the app starts with: the one chosen on
// the Station page, else CB_BACKEND, else the default. It puts that
// kind's inbox cursor in place, so it runs before anything reads the
// inbox.
func chooseRadio(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (radiokind.Kind, error) {
	id, err := st.RadioKind(ctx)
	if err != nil {
		return radiokind.Kind{}, fmt.Errorf("read radio kind: %w", err)
	}
	if id == "" {
		id = cfg.Backend
	}
	if id == "" {
		id = radiokind.Default
	}
	kind, ok := radiokind.Lookup(id)
	if !ok { // saved by a newer build: run, so the Station page can change it
		logger.Error("unknown radio kind; using the default", "kind", id, "default", radiokind.Default)
		kind, _ = radiokind.Lookup(radiokind.Default)
	}
	if kind.NeedsGraywolf && !cfg.GraywolfReady() {
		// No graywolf login (e.g. a node first set up as a UV-K5 node):
		// stay up on a radio that needs none, so the Station page can be
		// reached, rather than failing to start.
		other, ok := withoutGraywolf()
		if !ok {
			return radiokind.Kind{}, fmt.Errorf("radio %q needs graywolf's login (%s and %s or %s): run install.sh again",
				kind.ID, config.EnvGraywolfUser, config.EnvGraywolfPassword, config.EnvGraywolfPassFile)
		}
		logger.Error("this radio kind needs graywolf's login, which isn't set; using another",
			"kind", kind.ID, "using", other.ID, "fix", "set GW_USER and GW_PASSWORD_FILE, or run install.sh again")
		kind = other
	}
	changed, err := st.ActivateRadioKind(ctx, kind.ID)
	if err != nil {
		return radiokind.Kind{}, fmt.Errorf("activate radio kind: %w", err)
	}
	if changed {
		logger.Warn("radio kind changed; reports go out through the new radio", "kind", kind.ID)
	}
	return kind, nil
}

// withoutGraywolf is the first radio kind that doesn't need graywolf.
func withoutGraywolf() (radiokind.Kind, bool) {
	for _, k := range radiokind.All() {
		if !k.NeedsGraywolf {
			return k, true
		}
	}
	return radiokind.Kind{}, false
}

// startMessaging connects the chosen backend; its loops run until ctx ends.
func startMessaging(ctx context.Context, cfg config.Config, kind radiokind.Kind, st *store.Store, logger *slog.Logger) (messaging, error) {
	var (
		m   messaging
		err error
	)
	switch kind.ID {
	case radiokind.UVK5:
		m, err = startRadio(ctx, cfg, st, logger)
	case radiokind.Graywolf:
		m, err = startGraywolf(ctx, cfg, st, logger)
	default:
		err = fmt.Errorf("radio kind %q has no backend", kind.ID)
	}
	m.kind = kind
	return m, err
}

func startGraywolf(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (messaging, error) {
	gw, err := graywolf.New(graywolf.Config{
		BaseURL: cfg.GraywolfURL, Username: cfg.GraywolfUser, Password: cfg.GraywolfPassword,
		Timeout: cfg.Timeout, Logger: logger,
	})
	if err != nil {
		return messaging{}, fmt.Errorf("create graywolf client: %w", err)
	}
	checkGraywolf(ctx, gw, logger)
	// The cable watcher holds race reports while graywolf's radio cable
	// (its sound card) is missing.
	cable := cablewatch.New(cablewatch.Config{Graywolf: gw, Settings: st.GetSettings,
		Cards: func() ([]hostmon.SoundCard, bool) { return hostmon.ReadSoundCards("/proc") },
		Local: cfg.GraywolfLocal(), Logger: logger})
	go cable.Run(ctx, cablewatch.Every)
	return messaging{gw: gw, hold: cable.HoldReason, cable: cable, local: cfg.GraywolfLocal()}, nil
}

// startRadio connects a UV-K5/UV-K1 radio on USB-C (CB_BACKEND=uvk5).
func startRadio(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (messaging, error) {
	tools := &radioTools{port: cfg.UVK5Port, log: logger}
	rb, err := radiobackend.New(radiobackend.Config{Store: st, Logger: logger, Now: time.Now,
		Dial: func(ctx context.Context) (radiobackend.Session, error) {
			sess, port, err := uvk5.Dial(ctx, cfg.UVK5Port)
			if err != nil {
				return nil, err
			}
			logger.Info("radio found", "port", port)
			tools.last.Store(port)
			return sess, nil
		}})
	if err != nil {
		return messaging{}, err
	}
	tools.rb = rb
	go rb.Run(ctx)
	return messaging{gw: rb, hold: radioHold(rb.State), radio: rb, tools: tools}, nil
}

// radioHold holds race reports while the radio isn't connected (or is
// lent to UV Studio or the firmware flasher).
func radioHold(state func() radiobackend.State) func() string {
	return func() string {
		switch st := state(); {
		case st.Lent != "":
			return "radio in use: " + st.Lent
		case !st.Connected:
			return "radio not connected"
		}
		return ""
	}
}

// slots, firmwareDir and bridge are the UV-K5 extras for the web
// pages (nil on a graywolf node: the pages say they're UV-K5 only).
func (m messaging) slots(dir string) web.SlotWriter {
	if m.tools == nil {
		return nil
	}
	return m.tools.newSlots(dir)
}

func (m messaging) firmwareDir(dir string) string {
	if m.tools == nil {
		return ""
	}
	return dir
}

func (m messaging) bridge() web.RadioBridge {
	if m.tools == nil {
		return nil
	}
	return m.tools
}
