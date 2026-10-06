// Command checkin-board runs race checkpoint reporting next to a
// graywolf station, talking to it only through graywolf's REST API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"checkin-board/internal/config"
	"checkin-board/internal/graywolf"
)

// testedGraywolfVersion is the graywolf release this build was
// contract-tested against.
const testedGraywolfVersion = "0.14.14"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger.Info("starting", "config", cfg.String())

	gw, err := graywolf.New(graywolf.Config{
		BaseURL:  cfg.GraywolfURL,
		Username: cfg.GraywolfUser,
		Password: cfg.GraywolfPassword,
		Timeout:  cfg.Timeout,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("create graywolf client: %w", err)
	}

	ver, err := gw.Version(ctx)
	if err != nil {
		return fmt.Errorf("graywolf version: %w", err)
	}
	if ver.Version != testedGraywolfVersion {
		logger.Warn("untested graywolf version", "have", ver.Version, "tested", testedGraywolfVersion)
	}
	station, err := gw.StationConfig(ctx)
	if err != nil {
		return fmt.Errorf("graywolf station config: %w", err)
	}
	logger.Info("graywolf ready", "version", ver.Version, "callsign", station.Callsign)

	// TODO(phase 4+): start the inbox reader, engines and web server.
	return nil
}
