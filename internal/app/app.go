package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"checkin-board/internal/checkpoint"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/hq"
	"checkin-board/internal/inbox"
	"checkin-board/internal/ops"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

// prefsRefresh is how often graywolf's message preferences (its DM
// length limit) are re-read; prefsRetry after a failure (e.g. graywolf
// still starting).
const (
	prefsRefresh = 5 * time.Minute
	prefsRetry   = 30 * time.Second
)

// Graywolf is everything the app needs from the graywolf client.
type Graywolf interface {
	checkpoint.Messages
	inbox.Graywolf
	peers.Prefs
	cleanupClient
	MessagePreferences(ctx context.Context) (graywolf.MessagePreferences, error)
}

// Config configures an App.
type Config struct {
	Store    *store.Store
	Graywolf Graywolf
	Clock    *raceclock.Clock // nil: browser-synced clock with no OS check
	Logger   *slog.Logger
	// DataDir holds the bib journal and reset backups (normally the
	// database's directory).
	DataDir string
	// StartedAt is saved, on a node's first run only, as the inbox
	// reader's starting point, so a fresh node doesn't replay old races
	// from graywolf's history. Later runs keep the first value.
	StartedAt time.Time
}

// App is the assembled service.
type App struct {
	cfg        Config
	log        *slog.Logger
	Clock      *raceclock.Clock
	Peers      *peers.Ensurer
	Checkpoint *checkpoint.Engine
	HQ         *hq.Engine
	Inbox      *inbox.Reader
	Ops        *ops.Service
}

// Graywolf also needs DeleteMessage for post-race cleanup.
type cleanupClient interface {
	DeleteMessage(ctx context.Context, id uint64) error
}

// New wires the components. It does not start anything.
func New(cfg Config) (*App, error) {
	if cfg.Store == nil || cfg.Graywolf == nil {
		return nil, errors.New("app: Store and Graywolf are required")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = raceclock.NewClock(nil, nil)
	}
	if cfg.DataDir == "" {
		return nil, errors.New("app: DataDir is required")
	}
	ensurer := peers.NewEnsurer(cfg.Graywolf, cfg.Store, nil)
	var opsSvc *ops.Service // set below; the check-in hook needs it
	cp, err := checkpoint.New(checkpoint.Config{
		Store: cfg.Store, Graywolf: cfg.Graywolf, Clock: clock, Peers: ensurer, Logger: log,
		OnCheckedIn: func(ctx context.Context) error {
			if opsSvc == nil {
				return errors.New("app: not ready")
			}
			return opsSvc.RestorePeers(ctx)
		},
	})
	if err != nil {
		return nil, err
	}
	hqe, err := hq.New(hq.Config{Store: cfg.Store, Graywolf: cfg.Graywolf, Clock: clock, Peers: ensurer, Logger: log})
	if err != nil {
		return nil, err
	}
	reader, err := inbox.New(inbox.Config{
		Graywolf: cfg.Graywolf, Store: cfg.Store, Dispatcher: NewDispatcher(cfg.Store, cp, hqe), Logger: log,
	})
	if err != nil {
		return nil, err
	}
	opsSvc, err = ops.New(ops.Config{
		Store: cfg.Store, Graywolf: cfg.Graywolf, Clock: clock, Peers: ensurer, Checkpoint: cp, HQ: hqe,
		JournalPath: filepath.Join(cfg.DataDir, "race-journal.csv"),
		BackupDir:   filepath.Join(cfg.DataDir, "backups"),
		Logger:      log,
	})
	if err != nil {
		return nil, err
	}
	return &App{cfg: cfg, log: log, Clock: clock, Peers: ensurer, Checkpoint: cp, HQ: hqe, Inbox: reader, Ops: opsSvc}, nil
}

// Close releases what the app holds open (the bib journal).
func (a *App) Close() error {
	return a.Ops.Close()
}

// Run starts every component and blocks until ctx is done. On a
// checkpoint it first recovers batches whose send outcome was lost in a
// crash (spec 4.1.7).
//
// Startup recovery is best effort: the checkpoint engine repeats it on
// any tick that finds an unbound batch, so graywolf being late is fine.
func (a *App) Run(ctx context.Context) error {
	if !a.cfg.StartedAt.IsZero() {
		if err := a.cfg.Store.EnsureInboxSince(ctx, a.cfg.StartedAt); err != nil {
			return fmt.Errorf("app: save inbox starting point: %w", err)
		}
	}
	if err := a.recover(ctx); err != nil {
		a.log.Warn("app: startup recovery", "err", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = a.Inbox.Run(ctx) })
	wg.Go(func() { _ = a.Checkpoint.Run(ctx) })
	wg.Go(func() { _ = a.HQ.Run(ctx) })
	wg.Go(func() { a.refreshPrefs(ctx) })
	wg.Wait()
	return ctx.Err()
}

func (a *App) recover(ctx context.Context) error {
	cfg, err := a.cfg.Store.GetSettings(ctx)
	if err != nil {
		return err
	}
	if cfg.Role != store.RoleCheckpoint {
		return nil
	}
	return a.Checkpoint.Recover(ctx, cfg)
}

// refreshPrefs keeps both engines' view of graywolf's DM length limit
// current, so batches never exceed what graywolf accepts.
func (a *App) refreshPrefs(ctx context.Context) {
	for {
		wait := prefsRefresh
		if err := a.RefreshPrefs(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("app: read graywolf message preferences", "err", err)
			wait = prefsRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// RefreshPrefs reads graywolf's message preferences once.
func (a *App) RefreshPrefs(ctx context.Context) error {
	p, err := a.cfg.Graywolf.MessagePreferences(ctx)
	if err != nil {
		return fmt.Errorf("app: %w", err)
	}
	a.Checkpoint.SetGraywolfMaxText(p.MaxText())
	a.HQ.SetGraywolfMaxText(p.MaxText())
	return nil
}
