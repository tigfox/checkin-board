// Package ops is the operator-facing layer the REST API calls: keypad
// logging with the power-safe journal, checkpoint export and HQ import,
// the HQ board and results, and the race lifecycle (spec 4.4, 4.7).
// Much of it is ported from graywolf pkg/race (our own code).
package ops

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/hq"
	"checkin-board/internal/journal"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

var (
	// ErrWrongRole: the operation belongs to the other role (or none).
	ErrWrongRole = errors.New("ops: not available in this node's role")
	// ErrWrongState: the operation isn't allowed in the race's current
	// lifecycle state.
	ErrWrongState = errors.New("ops: not allowed in the current race state")
	// ErrJournalOnly: the database write failed but the entry is safe in
	// the bib journal. The keypad must tell the volunteer NOT to retype
	// it (that would double it); the journal can be imported at HQ.
	ErrJournalOnly = errors.New("ops: saved to the backup journal only (database error); do not re-enter")
	// ErrConfirmMismatch: a destructive action's typed confirmation
	// didn't match.
	ErrConfirmMismatch = errors.New("ops: confirmation text does not match")
	// ErrUnsentData: a reset would discard entries HQ hasn't confirmed;
	// it needs an explicit acknowledgement (or a final check-in first).
	ErrUnsentData = errors.New("ops: this node has entries HQ has not confirmed")
)

// Graywolf is what ops needs from the graywolf client: deleting race
// rows after the race, and restoring per-peer settings.
type Graywolf interface {
	peers.Prefs
	GetMessage(ctx context.Context, id uint64) (graywolf.Message, error)
	DeleteMessage(ctx context.Context, id uint64) error
}

// resettable is an engine with in-memory race state.
type resettable interface{ Reset() }

// Config configures a Service.
type Config struct {
	Store    *store.Store
	Graywolf Graywolf
	Clock    *raceclock.Clock
	Peers    *peers.Ensurer
	// Checkpoint and HQ engines, reset with the node.
	Checkpoint resettable
	HQ         *hq.Engine
	// JournalPath is the bib journal file; empty disables the journal.
	JournalPath string
	// BackupDir is where Reset writes its backups.
	BackupDir string
	Now       func() time.Time // nil: time.Now
	Logger    *slog.Logger
}

// Service implements the operator actions.
type Service struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	journalMu    sync.Mutex
	journal      *journal.Journal
	journalErr   string
	journalErrAt time.Time
}

// New validates cfg and returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Graywolf == nil || cfg.Clock == nil || cfg.Peers == nil ||
		cfg.Checkpoint == nil || cfg.HQ == nil || cfg.BackupDir == "" {
		return nil, errors.New("ops: Store, Graywolf, Clock, Peers, engines and BackupDir are required")
	}
	s := &Service{cfg: cfg, log: cfg.Logger, now: cfg.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s, nil
}

// Close closes the journal.
func (s *Service) Close() error {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	if s.journal == nil {
		return nil
	}
	err := s.journal.Close()
	s.journal = nil
	return err
}

func (s *Service) settings(ctx context.Context) (store.Settings, error) {
	return s.cfg.Store.GetSettings(ctx)
}
