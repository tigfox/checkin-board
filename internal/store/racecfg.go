package store

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// MaxEventPageBytes caps the event page (phase 12a).
const MaxEventPageBytes = 64 << 10

type eventPageRow struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	Text      string    `gorm:"column:text"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (eventPageRow) TableName() string { return "event_page" }

type graywolfBackupRow struct {
	ID      uint      `gorm:"column:id;primaryKey"`
	Data    string    `gorm:"column:data"`
	SavedAt time.Time `gorm:"column:saved_at"`
}

func (graywolfBackupRow) TableName() string { return "graywolf_backup" }

// EventPage returns the event page's text and when it was last set (nil
// when never). It is text the operator writes (or a race config sets):
// shown as text, never as HTML.
func (s *Store) EventPage(ctx context.Context) (string, *time.Time, error) {
	var r eventPageRow
	err := s.db.WithContext(ctx).Take(&r, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	at := normTime(r.UpdatedAt)
	return r.Text, &at, nil
}

// SetEventPage replaces the event page's text.
func (s *Store) SetEventPage(ctx context.Context, text string) error {
	if len(text) > MaxEventPageBytes || !utf8.ValidString(text) {
		return fmt.Errorf("%w: the event page must be UTF-8 text of at most %d KB", ErrInvalidInput, MaxEventPageBytes>>10)
	}
	r := eventPageRow{ID: 1, Text: text, UpdatedAt: normTime(s.now())}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error
}

// GraywolfBackup returns graywolf's settings as saved before a race
// config first changed them ("" when there is no backup).
func (s *Store) GraywolfBackup(ctx context.Context) (string, *time.Time, error) {
	var r graywolfBackupRow
	err := s.db.WithContext(ctx).Take(&r, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	at := normTime(r.SavedAt)
	return r.Data, &at, nil
}

// PutGraywolfBackup replaces the saved graywolf settings. The caller
// merges, so the first-saved original of each value is kept.
func (s *Store) PutGraywolfBackup(ctx context.Context, data string) error {
	r := graywolfBackupRow{ID: 1, Data: data, SavedAt: normTime(s.now())}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error
}

// ClearGraywolfBackup drops the backup once graywolf's settings are
// restored.
func (s *Store) ClearGraywolfBackup(ctx context.Context) error {
	return s.db.WithContext(ctx).Delete(&graywolfBackupRow{}, 1).Error
}

// ReplaceCheckpoints replaces HQ's checkpoint list with list, in one
// transaction: an invalid row changes nothing.
func (s *Store) ReplaceCheckpoints(ctx context.Context, list []Checkpoint) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return s.replaceCheckpointsTx(tx, list) })
}

func (s *Store) replaceCheckpointsTx(tx *gorm.DB, list []Checkpoint) error {
	for _, c := range list {
		if err := c.Validate(); err != nil {
			return err
		}
	}
	now := normTime(s.now())
	if err := tx.Exec("DELETE FROM checkpoints").Error; err != nil {
		return err
	}
	for _, c := range list {
		row := c
		row.ID, row.CreatedAt, row.UpdatedAt = 0, now, now
		if err := tx.Create(&row).Error; err != nil {
			return mapDBError(err)
		}
	}
	return nil
}

// ApplyRaceConfig writes what a race config sets on this node in one
// transaction, only before the race starts (ErrNotSetup otherwise):
// settings, HQ's checkpoint list (nil: unchanged) and the event page
// (nil: unchanged). Nothing is written if any part fails.
func (s *Store) ApplyRaceConfig(ctx context.Context, c Settings, checkpoints []Checkpoint, eventPage *string) error {
	if eventPage != nil && (len(*eventPage) > MaxEventPageBytes || !utf8.ValidString(*eventPage)) {
		return fmt.Errorf("%w: the event page must be UTF-8 text of at most %d KB", ErrInvalidInput, MaxEventPageBytes>>10)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := s.updateSettingsTx(tx, c, true); err != nil {
			return err
		}
		if checkpoints != nil {
			if err := s.replaceCheckpointsTx(tx, checkpoints); err != nil {
				return err
			}
		}
		if eventPage != nil {
			r := eventPageRow{ID: 1, Text: *eventPage, UpdatedAt: normTime(s.now())}
			if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&r).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
