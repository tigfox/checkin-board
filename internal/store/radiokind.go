package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// RadioIDBase is the first radio message id: radio rows never share an
// id with graywolf's (migration 0010).
const RadioIDBase uint64 = 1 << 40

type nodeRadio struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	Kind       string    `gorm:"column:kind"`
	ActiveKind string    `gorm:"column:active_kind"`
	UpdatedAt  time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (nodeRadio) TableName() string { return "node_radio" }

type inboxCursor struct {
	Kind   string `gorm:"column:kind;primaryKey"`
	Cursor string `gorm:"column:cursor"`
}

func (inboxCursor) TableName() string { return "inbox_cursors" }

// RadioKind returns the radio kind the operator chose ("" if never).
func (s *Store) RadioKind(ctx context.Context) (string, error) {
	var nr nodeRadio
	err := s.db.WithContext(ctx).First(&nr, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return nr.Kind, err
}

// SetRadioKind saves the operator's choice; the app uses it from its
// next start (ActivateRadioKind). The caller validates kind.
func (s *Store) SetRadioKind(ctx context.Context, kind string) error {
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return fmt.Errorf("%w: radio kind is required", ErrInvalidInput)
	}
	row := nodeRadio{ID: 1, Kind: kind, UpdatedAt: normTime(s.now())}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"kind", "updated_at"}),
	}).Create(&row).Error
}

// ActivateRadioKind is run at startup, before anything reads the inbox,
// with the kind the app is starting with. When that differs from the
// kind it last started with, the inbox cursor is swapped: the old
// kind's is put aside and the new kind's (or none: read from the
// starting point) restored. It reports whether the kind changed.
func (s *Store) ActivateRadioKind(ctx context.Context, kind string) (bool, error) {
	if strings.TrimSpace(kind) == "" {
		return false, fmt.Errorf("%w: radio kind is required", ErrInvalidInput)
	}
	changed := false
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var nr nodeRadio
		err := tx.First(&nr, 1).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		prev := nr.ActiveKind
		if prev == kind {
			return nil
		}
		if prev != "" { // "" (first start that knows kinds): the cursor is this kind's already
			changed = true
			if err := swapCursor(tx, prev, kind, s.now()); err != nil {
				return err
			}
		}
		if nr.Kind == "" {
			nr.Kind = kind
		}
		nr.ID, nr.ActiveKind, nr.UpdatedAt = 1, kind, normTime(s.now())
		return tx.Save(&nr).Error
	})
	return changed, err
}

func swapCursor(tx *gorm.DB, from, to string, now time.Time) error {
	var st inboxState
	err := tx.First(&st, 1).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err := tx.Save(&inboxCursor{Kind: from, Cursor: st.Cursor}).Error; err != nil {
		return err
	}
	var next inboxCursor
	err = tx.First(&next, "kind = ?", to).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	st.ID, st.Cursor, st.UpdatedAt = 1, next.Cursor, normTime(now)
	return tx.Save(&st).Error
}
