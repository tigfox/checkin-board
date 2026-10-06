package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/peers"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// Lifecycle (spec 4.7):
//
//	setup ─Start→ active ─Complete→ complete
//	checkpoint: active|complete|checking_in ─Secure→ secured (packed up, radio quiet)
//	            secured|complete ─CheckIn→ checking_in ─(all confirmed)→ checked_in
//	any ─Reset→ setup

// transition moves the race from one of `from` to `to` with a
// compare-and-set, or fails with ErrWrongState; concurrent changes (the
// engine finishing a check-in, another admin) can't be overwritten.
// roleOK restricts which roles may do it.
func (s *Service) transition(ctx context.Context, roleOK func(string) bool, from []string, to string, startedAt *time.Time) (store.Settings, error) {
	cfg, err := s.settings(ctx)
	if err != nil {
		return store.Settings{}, err
	}
	if !roleOK(cfg.Role) {
		return store.Settings{}, ErrWrongRole
	}
	ok, err := s.cfg.Store.SetRaceState(ctx, from, to, startedAt)
	if err != nil {
		return store.Settings{}, err
	}
	if !ok {
		return store.Settings{}, fmt.Errorf("%w: can't go from %s to %s", ErrWrongState, cfg.RaceState, to)
	}
	s.log.Info("ops: race state changed", "from", cfg.RaceState, "to", to)
	return s.settings(ctx)
}

var allStates = []string{store.RaceSetup, store.RaceActive, store.RaceComplete,
	store.RaceSecured, store.RaceCheckingIn, store.RaceCheckedIn}

func anyRole(r string) bool        { return r != store.RoleUnset }
func checkpointRole(r string) bool { return r == store.RoleCheckpoint }

// Start opens the race: the keypad starts taking entries.
func (s *Service) Start(ctx context.Context) (store.Settings, error) {
	now, _ := s.cfg.Clock.Now()
	return s.transition(ctx, anyRole, []string{store.RaceSetup}, store.RaceActive, &now)
}

// Complete closes the keypad. A checkpoint keeps delivering its backlog;
// HQ keeps requesting missing batches, so later check-ins still work.
func (s *Service) Complete(ctx context.Context) (store.Settings, error) {
	return s.transition(ctx, anyRole, []string{store.RaceActive}, store.RaceComplete, nil)
}

// SecureResult reports what a secured checkpoint still owes HQ.
type SecureResult struct {
	Settings    store.Settings
	Unconfirmed int // entries to deliver at the final check-in
}

// Secure packs a checkpoint up for the trip back to HQ: the keypad is
// closed and nothing more is transmitted until the final check-in.
func (s *Service) Secure(ctx context.Context) (SecureResult, error) {
	cfg, err := s.transition(ctx, checkpointRole,
		[]string{store.RaceActive, store.RaceComplete, store.RaceCheckingIn}, store.RaceSecured, nil)
	if err != nil {
		return SecureResult{}, err
	}
	st, err := s.cfg.Store.OutboxStats(ctx)
	if err != nil {
		return SecureResult{}, err
	}
	return SecureResult{Settings: cfg, Unconfirmed: st.Unconfirmed}, nil
}

// CheckIn starts the final check-in at HQ: everything HQ hasn't
// confirmed goes out at once, and the node moves to checked_in by
// itself when HQ has it all. If RF can't finish it, ExportCheckpoint is
// the offline path.
func (s *Service) CheckIn(ctx context.Context) (store.Settings, error) {
	return s.transition(ctx, checkpointRole, []string{store.RaceSecured, store.RaceComplete}, store.RaceCheckingIn, nil)
}

// RestorePeers puts graywolf's per-peer settings back to what they were
// before the race (spec 3.3) and forgets the cached "already set" state.
func (s *Service) RestorePeers(ctx context.Context) error {
	err := peers.Restore(ctx, s.cfg.Graywolf, s.cfg.Store)
	s.cfg.Peers.Reset()
	if err != nil {
		s.log.Warn("ops: restore graywolf peer settings", "err", err)
	}
	return err
}

// CleanupResult reports what graywolf cleanup did.
type CleanupResult struct {
	Deleted int // rows deleted from graywolf
	Gone    int // already gone (deleted by the operator)
	Kept    int // still needed: batches HQ hasn't confirmed
	Failed  int // graywolf errors; run cleanup again
}

// cleanupStates are where graywolf cleanup is allowed: after the race.
var cleanupStates = map[string]map[string]bool{
	// Not during a check-in: it could delete a heartbeat still in flight.
	store.RoleCheckpoint: {store.RaceComplete: true, store.RaceSecured: true, store.RaceCheckedIn: true},
	store.RoleHQ:         {store.RaceComplete: true},
}

// CleanupGraywolf deletes from graywolf the race messages the app
// recorded (spec 4.7.3), one row at a time. It only deletes rows whose
// ids the app stored and whose text starts "RC1 ", never whole threads,
// and never a row whose batch HQ hasn't confirmed (deleting it would
// cancel its resend). Repeatable: a 404 counts as already gone.
func (s *Service) CleanupGraywolf(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	cfg, err := s.settings(ctx)
	if err != nil {
		return res, err
	}
	if !cleanupStates[cfg.Role][cfg.RaceState] {
		return res, fmt.Errorf("%w: graywolf cleanup runs only after the race is complete", ErrWrongState)
	}
	rows, err := s.cfg.Store.ListGWRows(ctx)
	if err != nil {
		return res, err
	}
	keep, err := s.cfg.Store.UnconfirmedRows(ctx)
	if err != nil {
		return res, err
	}
	for _, r := range rows {
		if keep[r.GWMessageID] {
			res.Kept++
			continue
		}
		switch err := s.deleteRaceRow(ctx, r.GWMessageID); {
		case errors.Is(err, errRowGone):
			res.Gone++
		case err != nil:
			res.Failed++
			s.log.Warn("ops: graywolf cleanup", "row", r.GWMessageID, "err", err)
			continue
		default:
			res.Deleted++
		}
		if err := s.cfg.Store.MarkGWRowDeleted(ctx, r.GWMessageID, s.now()); err != nil {
			return res, err
		}
	}
	return res, nil
}

var errRowGone = errors.New("ops: graywolf row already gone")

func (s *Service) deleteRaceRow(ctx context.Context, id uint64) error {
	m, err := s.cfg.Graywolf.GetMessage(ctx, id)
	if graywolf.IsNotFound(err) {
		return errRowGone
	}
	if err != nil {
		return err
	}
	if !wire.IsRaceText(m.Text) {
		return fmt.Errorf("row %d is not race traffic; left alone", id)
	}
	if err := s.cfg.Graywolf.DeleteMessage(ctx, id); err != nil {
		if graywolf.IsNotFound(err) {
			return errRowGone
		}
		return err
	}
	return nil
}

// ResetRequest confirms a reset.
type ResetRequest struct {
	// Confirm must equal the race name (or "RESET" if it has none).
	Confirm string
	// ClearReference also clears HQ's checkpoint list and roster.
	ClearReference bool
	// AcknowledgeUnsent allows resetting a checkpoint that still has
	// entries HQ hasn't confirmed (they are kept in the backup).
	AcknowledgeUnsent bool
}

// ResetResult reports where the backup went and anything that needs
// the operator's attention.
type ResetResult struct {
	BackupDir string
	Warnings  []string
}

// Reset clears all race data on this node so it can be reused (spec
// 4.7.4). It always writes a backup first: a database snapshot, the
// checkpoint export or HQ results, and the bib journal (moved, so the
// next race starts a fresh one). It restores graywolf's peer settings,
// moves the inbox reader's starting point to now, and clears the race
// clock sync and the engines' in-memory state. Settings are kept.
func (s *Service) Reset(ctx context.Context, req ResetRequest) (ResetResult, error) {
	var res ResetResult
	cfg, err := s.settings(ctx)
	if err != nil {
		return res, err
	}
	want := cfg.RaceName
	if want == "" {
		want = "RESET"
	}
	if !strings.EqualFold(strings.TrimSpace(req.Confirm), want) {
		return res, fmt.Errorf("%w: type %q to confirm", ErrConfirmMismatch, want)
	}
	if cfg.Role == store.RoleCheckpoint && cfg.RaceState != store.RaceCheckedIn && !req.AcknowledgeUnsent {
		st, err := s.cfg.Store.OutboxStats(ctx)
		if err != nil {
			return res, err
		}
		if st.Unconfirmed > 0 {
			return res, fmt.Errorf("%w: %d entries; do a final check-in or export first", ErrUnsentData, st.Unconfirmed)
		}
	}
	// The rest runs to completion even if the request goes away.
	ctx = context.WithoutCancel(ctx)
	// Close the keypad and stop the engines first, so nothing is logged
	// or sent between the backup and the wipe. If a later step fails, the
	// data is still there and the reset can simply be retried.
	if _, err := s.cfg.Store.SetRaceState(ctx, allStates, store.RaceSetup, nil); err != nil {
		return res, err
	}
	if res.BackupDir, res.Warnings, err = s.backup(ctx, cfg); err != nil {
		return res, err
	}
	if err := s.cfg.Store.ResetRaceData(ctx, store.ResetOptions{ClearReference: req.ClearReference}); err != nil {
		return res, err
	}
	// Only once the data is gone: a fresh journal for the next race.
	if err := s.rotateJournal(res.BackupDir); err != nil {
		res.Warnings = append(res.Warnings, "bib journal not moved into the backup: "+err.Error())
	}
	if err := s.RestorePeers(ctx); err != nil {
		res.Warnings = append(res.Warnings, "graywolf peer settings not restored (retry from the admin page): "+err.Error())
	}
	if err := s.cfg.Store.SetInboxSince(ctx, s.now()); err != nil {
		return res, err
	}
	s.cfg.Clock.Reset()
	s.cfg.Checkpoint.Reset()
	s.cfg.HQ.Reset()
	s.log.Info("ops: node reset", "backup", res.BackupDir)
	return res, nil
}

// backup writes the pre-reset backup and returns its directory.
// Failure to snapshot the database aborts the reset; a failed export or
// journal move is reported as a warning (the snapshot holds the data).
func (s *Service) backup(ctx context.Context, cfg store.Settings) (string, []string, error) {
	dir := filepath.Join(s.cfg.BackupDir, slug(cfg.RaceName)+"-"+s.now().UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, fmt.Errorf("ops: create backup dir: %w", err)
	}
	if err := s.cfg.Store.Backup(ctx, filepath.Join(dir, "checkin-board.db")); err != nil {
		return "", nil, err
	}
	var warnings []string
	switch cfg.Role {
	case store.RoleCheckpoint:
		if err := writeFile(filepath.Join(dir, "export.csv"), func(f *os.File) error {
			_, err := s.ExportCheckpoint(ctx, f)
			return err
		}); err != nil {
			warnings = append(warnings, "checkpoint export not written: "+err.Error())
		}
	case store.RoleHQ:
		if err := writeFile(filepath.Join(dir, "results.csv"), func(f *os.File) error {
			return s.ExportResults(ctx, f)
		}); err != nil {
			warnings = append(warnings, "results export not written: "+err.Error())
		}
	}
	return dir, warnings, nil
}

// rotateJournal closes the journal and moves it into dir, so the next
// race starts a fresh one.
func (s *Service) rotateJournal(dir string) error {
	if s.cfg.JournalPath == "" {
		return nil
	}
	s.journalMu.Lock()
	defer s.journalMu.Unlock()
	if s.journal != nil {
		_ = s.journal.Close()
		s.journal = nil
	}
	err := os.Rename(s.cfg.JournalPath, filepath.Join(dir, "race-journal.csv"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func writeFile(path string, fill func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := fill(f); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// slug makes a race name safe as a directory name.
func slug(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	if s := strings.Trim(b.String(), "-"); s != "" && len(s) <= 40 {
		return s
	} else if s != "" {
		return s[:40]
	}
	return "race"
}
