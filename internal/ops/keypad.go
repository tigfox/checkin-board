package ops

import (
	"context"
	"fmt"
	"slices"
	"time"

	"checkin-board/internal/journal"
	"checkin-board/internal/store"
)

// LogResult is what LogBib recorded.
type LogResult struct {
	ID          uint // LocalEntry.ID on a checkpoint, ReceivedEntry.ID on HQ
	TimeIn      time.Time
	ClockSynced bool
	// JournalErr is set when the entry was stored but could not be
	// written to the journal. Logging still succeeded.
	JournalErr error
}

// JournalStatus reports the bib journal's health for the UI.
type JournalStatus struct {
	Enabled     bool
	Path        string
	LastError   string
	LastErrorAt *time.Time
}

// KeypadEntry is one row of the keypad's recent-entries list.
type KeypadEntry struct {
	ID          uint
	CP          string
	Bib         store.Bib
	TimeIn      time.Time
	ClockSynced bool
	State       string // queued / sent / confirmed (checkpoint), recorded (HQ)
	Voided      bool
	VoidState   string // delivery state of the void (checkpoint only)
}

// EntryRecorded is the keypad state of an HQ entry: it needs no delivery.
const EntryRecorded = "recorded"

// LogBib records a bib passing local checkpoint cp at the race clock's
// current time. Only while the race is active. The journal line is
// written (and fsynced) before the database row, so the press survives
// even if the DB write fails.
func (s *Service) LogBib(ctx context.Context, cp string, bib store.Bib) (LogResult, error) {
	cfg, err := s.settings(ctx)
	if err != nil {
		return LogResult{}, err
	}
	if cfg.Role == store.RoleUnset {
		return LogResult{}, ErrWrongRole
	}
	if cfg.RaceState != store.RaceActive {
		return LogResult{}, fmt.Errorf("%w: the keypad is open only while the race is active", ErrWrongState)
	}
	if !slices.Contains(cfg.LocalCodes(), cp) {
		return LogResult{}, fmt.Errorf("%w: %q is not a local checkpoint code", store.ErrInvalidInput, cp)
	}
	if !bib.Valid() {
		return LogResult{}, fmt.Errorf("%w: bib %d", store.ErrInvalidInput, bib)
	}
	now, synced := s.cfg.Clock.Now()
	timeIn := now.UTC().Truncate(time.Second)
	res := LogResult{TimeIn: timeIn, ClockSynced: synced}
	res.JournalErr = s.journalAppend(journal.Record{
		LoggedAt: now, Event: journal.EventEntry, CP: cp, Bib: bib, TimeIn: timeIn, ClockSynced: synced,
	})
	var id uint
	if cfg.Role == store.RoleCheckpoint {
		var e *store.LocalEntry
		if e, err = s.cfg.Store.LogLocal(ctx, cp, bib, now, synced); err == nil {
			id = e.ID
		}
	} else {
		var e *store.ReceivedEntry
		if e, err = s.cfg.Store.LogHQLocal(ctx, cp, bib, now); err == nil {
			id = e.ID
		}
	}
	if err != nil {
		// Deliberately no compensating void: if the DB is failing, the
		// journal is the record. Report "journal only" so the keypad says
		// don't retype; only when the journal failed too is it plain.
		if res.JournalErr == nil && s.cfg.JournalPath != "" {
			s.log.Error("ops: database write failed; entry kept in the bib journal only", "cp", cp, "bib", bib, "err", err)
			return res, fmt.Errorf("%w: %v", ErrJournalOnly, err)
		}
		return res, err
	}
	res.ID = id
	return res, nil
}

// VoidResult is what VoidBib did.
type VoidResult struct {
	// JournalErr is set when the void was applied but not journaled; a
	// later journal import would then resurrect the entry.
	JournalErr error
}

// VoidBib cancels a keypad entry by id (a LocalEntry on a checkpoint, an
// HQ-local ReceivedEntry on HQ) and journals the void. Allowed while the
// race is active or complete (fixing a typo after closing). A void is
// journaled only after the DB accepts it, so a rejected void never
// lands in the journal, where an import would replay it.
func (s *Service) VoidBib(ctx context.Context, id uint) (VoidResult, error) {
	cfg, err := s.settings(ctx)
	if err != nil {
		return VoidResult{}, err
	}
	if cfg.RaceState != store.RaceActive && cfg.RaceState != store.RaceComplete {
		return VoidResult{}, fmt.Errorf("%w: entries can be voided only while the race is active or complete", ErrWrongState)
	}
	var rec journal.Record
	switch cfg.Role {
	case store.RoleCheckpoint:
		e, err := s.cfg.Store.GetLocalEntry(ctx, id)
		if err != nil {
			return VoidResult{}, err
		}
		if _, err := s.cfg.Store.VoidLocal(ctx, id); err != nil {
			return VoidResult{}, err
		}
		rec = journal.Record{CP: e.CPCode, Bib: e.Bib, TimeIn: e.TimeIn, ClockSynced: e.ClockSynced}
	case store.RoleHQ:
		e, err := s.cfg.Store.GetReceivedEntry(ctx, id)
		if err != nil {
			return VoidResult{}, err
		}
		now, _ := s.cfg.Clock.Now()
		if err := s.cfg.Store.VoidHQLocal(ctx, id, now); err != nil {
			return VoidResult{}, err
		}
		rec = journal.Record{CP: e.CPCode, Bib: e.Bib, TimeIn: e.TimeIn, ClockSynced: true}
	default:
		return VoidResult{}, ErrWrongRole
	}
	rec.LoggedAt, _ = s.cfg.Clock.Now()
	rec.Event = journal.EventVoid
	return VoidResult{JournalErr: s.journalAppend(rec)}, nil
}

// RecentEntries lists this node's latest keypad entries, newest first.
func (s *Service) RecentEntries(ctx context.Context, limit int) ([]KeypadEntry, error) {
	cfg, err := s.settings(ctx)
	if err != nil {
		return nil, err
	}
	switch cfg.Role {
	case store.RoleCheckpoint:
		views, err := s.cfg.Store.ListLocal(ctx, limit)
		if err != nil {
			return nil, err
		}
		out := make([]KeypadEntry, len(views))
		for i, v := range views {
			out[i] = KeypadEntry{ID: v.ID, CP: v.CPCode, Bib: v.Bib, TimeIn: v.TimeIn, ClockSynced: v.ClockSynced,
				State: v.State, Voided: v.Voided, VoidState: v.VoidState}
		}
		return out, nil
	case store.RoleHQ:
		hqe, err := s.cfg.Store.ListHQLocal(ctx, limit)
		if err != nil {
			return nil, err
		}
		out := make([]KeypadEntry, len(hqe))
		for i, e := range hqe {
			out[i] = KeypadEntry{ID: e.ID, CP: e.CP, Bib: e.Bib, TimeIn: e.TimeIn, ClockSynced: true,
				State: EntryRecorded, Voided: e.Voided}
		}
		return out, nil
	default:
		return nil, ErrWrongRole
	}
}

// ensureJournal opens the journal on first use; a failed open is
// retried on the next call (the disk may come back). Caller holds
// journalMu.
func (s *Service) ensureJournalLocked() error {
	if s.journal != nil || s.cfg.JournalPath == "" {
		return nil
	}
	j, err := journal.Open(s.cfg.JournalPath)
	if err != nil {
		return err
	}
	s.journal = j
	return nil
}

func (s *Service) journalAppend(r journal.Record) error {
	if s.cfg.JournalPath == "" {
		return nil
	}
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	err := s.ensureJournalLocked()
	if err == nil {
		err = s.journal.Append(r)
	}
	if err != nil {
		s.journalErr, s.journalErrAt = err.Error(), s.now()
		s.log.Error("ops: bib journal write failed; entry is in the database only", "err", err)
	}
	return err
}

// JournalStatus reports whether the journal is on and its last error.
func (s *Service) JournalStatus() JournalStatus {
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	st := JournalStatus{Enabled: s.cfg.JournalPath != "", Path: s.cfg.JournalPath, LastError: s.journalErr}
	if !s.journalErrAt.IsZero() {
		t := s.journalErrAt
		st.LastErrorAt = &t
	}
	return st
}
