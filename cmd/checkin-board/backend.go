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
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
	"checkin-board/internal/web"
)

// messaging is how this node sends race traffic: graywolf, or a UV-K5
// radio (phase 17), with what the app and the web pages need of it.
type messaging struct {
	gw interface {
		app.Graywolf
		web.Graywolf
	}
	hold  func() string    // why reports must wait ("": they needn't)
	cable web.CableStatus  // graywolf: the AIOC watcher
	radio radiocheck.Radio // radio node: the backend
	local bool             // graywolf runs on this machine
}

// startMessaging connects the chosen backend; its loops run until ctx ends.
func startMessaging(ctx context.Context, cfg config.Config, st *store.Store, logger *slog.Logger) (messaging, error) {
	if cfg.Radio() {
		return startRadio(ctx, cfg, st, logger)
	}
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
	rb, err := radiobackend.New(radiobackend.Config{Store: st, Logger: logger, Now: time.Now,
		Dial: func(ctx context.Context) (radiobackend.Session, error) {
			sess, port, err := uvk5.Dial(ctx, cfg.UVK5Port)
			if err != nil {
				return nil, err
			}
			logger.Info("radio found", "port", port)
			return sess, nil
		}})
	if err != nil {
		return messaging{}, err
	}
	go rb.Run(ctx)
	return messaging{gw: rb, hold: radioHold(rb.State), radio: rb}, nil
}

// radioHold holds race reports while the radio isn't connected.
func radioHold(state func() radiobackend.State) func() string {
	return func() string {
		if !state().Connected {
			return "radio not connected"
		}
		return ""
	}
}
