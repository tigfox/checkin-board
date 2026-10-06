// Command checkin-board runs race checkpoint reporting next to a
// graywolf station, talking to it only through graywolf's REST API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"checkin-board/internal/app"
	"checkin-board/internal/config"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
)

// testedGraywolfVersion is the graywolf release this build was
// contract-tested against.
const testedGraywolfVersion = "0.14.14"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startedAt := time.Now()

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
	checkGraywolf(ctx, gw, logger)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	a, err := app.New(app.Config{
		Store: st, Graywolf: gw, Logger: logger, StartedAt: startedAt,
		DataDir: filepath.Dir(cfg.DBPath),
	})
	if err != nil {
		return fmt.Errorf("assemble app: %w", err)
	}
	defer a.Close()
	// TODO(phase 9): start the web server.
	return a.Run(ctx)
}

// checkGraywolf logs graywolf's version and callsign. It never stops
// startup: graywolf may come up after us, and the engines retry.
func checkGraywolf(ctx context.Context, gw *graywolf.Client, logger *slog.Logger) {
	ver, err := gw.Version(ctx)
	if err != nil {
		logger.Warn("graywolf not reachable yet; will keep retrying", "err", err)
		return
	}
	if ver.Version != testedGraywolfVersion {
		logger.Warn("untested graywolf version", "have", ver.Version, "tested", testedGraywolfVersion)
	}
	station, err := gw.StationConfig(ctx)
	if err != nil {
		logger.Warn("read graywolf station config", "err", err)
		return
	}
	logger.Info("graywolf ready", "version", ver.Version, "callsign", station.Callsign)
}
