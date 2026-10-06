// Command checkin-board talks to a locally running API.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"checkin-board/internal/apiclient"
	"checkin-board/internal/config"
)

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

	client, err := apiclient.New(cfg.BaseURL, cfg.Timeout)
	if err != nil {
		return fmt.Errorf("create api client: %w", err)
	}

	logger.Info("checking api", "base_url", cfg.BaseURL)
	health, err := client.Health(ctx)
	if err != nil {
		return err
	}
	logger.Info("api reachable", "status", health.Status)

	// TODO: application logic goes here.
	return nil
}
