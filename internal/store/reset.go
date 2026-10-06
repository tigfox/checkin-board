package store

import (
	"context"
	"errors"
	"fmt"
	"os"

	"gorm.io/gorm"
)

// ResetOptions selects what Reset clears beyond the race data itself.
type ResetOptions struct {
	// ClearReference also clears HQ's checkpoint list and roster.
	ClearReference bool
	// ClearBranding also returns the status board to the default look.
	ClearBranding bool
}

// raceTables hold one race's data, in delete order.
var raceTables = []string{"local_entries", "batches", "received_entries", "received_batches", "cp_status", "bad_reports",
	"link_probes", "link_checks", "link_responses"}

// Backup writes a consistent snapshot of the database to path (SQLite
// VACUUM INTO). path must not exist.
func (s *Store) Backup(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: backup %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("store: backup: %w", err)
	}
	if err := s.db.WithContext(ctx).Exec("VACUUM INTO ?", path).Error; err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	return nil
}

// ResetRaceData clears every race's data (entries, batches, the HQ event
// log, checkpoint status, bad reports) in one transaction and returns
// the node to the setup state, keeping its settings. graywolf row
// records (gw_rows) are kept, so post-race cleanup still knows which
// graywolf messages are the app's. The caller backs up first and moves
// the inbox reader's starting point (spec 4.7.4).
func (s *Store) ResetRaceData(ctx context.Context, opts ResetOptions) error {
	tables := append([]string{}, raceTables...)
	if opts.ClearReference {
		tables = append(tables, "checkpoints", "runners")
	}
	if opts.ClearBranding {
		tables = append(tables, "branding", "branding_logo")
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, t := range tables {
			if err := tx.Exec("DELETE FROM " + t).Error; err != nil {
				return fmt.Errorf("store: reset %s: %w", t, err)
			}
		}
		return tx.Exec("UPDATE settings SET race_state = ?, race_started_at = NULL, updated_at = ?",
			RaceSetup, normTime(s.now())).Error
	})
}
