package store

import (
	"checkin-board/internal/wire"
	"context"
	"fmt"
	"strings"
	"unicode"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// HQ reference data: the course's checkpoints and the roster of valid bibs.

const (
	// MaxNameLen bounds checkpoint names.
	MaxNameLen = 64
	// MaxCategoryLen bounds a runner's category.
	MaxCategoryLen = 32
)

// Validate checks a checkpoint definition.
func (c Checkpoint) Validate() error {
	switch {
	case !wire.ValidCheckpointCode(c.Code):
		return fmt.Errorf("%w: checkpoint code %q", ErrInvalidInput, c.Code)
	case !validName(c.Name, MaxNameLen):
		return fmt.Errorf("%w: checkpoint name must be 1-%d printable characters", ErrInvalidInput, MaxNameLen)
	case c.CourseOrder < 0:
		return fmt.Errorf("%w: course order must be >= 0", ErrInvalidInput)
	case c.ExpectedCall != "" && !ValidStationCall(c.ExpectedCall):
		return fmt.Errorf("%w: expected callsign %q", ErrInvalidInput, c.ExpectedCall)
	}
	return nil
}

// Validate checks a roster row.
func (r Runner) Validate() error {
	switch {
	case !r.Bib.Valid():
		return fmt.Errorf("%w: bib %d", ErrInvalidInput, r.Bib)
	case r.Category != "" && !validName(r.Category, MaxCategoryLen):
		return fmt.Errorf("%w: category must be at most %d printable characters", ErrInvalidInput, MaxCategoryLen)
	}
	return nil
}

// validName accepts 1..maxLen bytes of printable text: no control
// characters and no bidirectional overrides (which can make a name
// render as something else on the board).
func validName(s string, maxLen int) bool {
	if strings.TrimSpace(s) == "" || len(s) > maxLen {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return false
		}
	}
	return true
}

// CreateCheckpoint inserts c, setting its ID and timestamps.
func (s *Store) CreateCheckpoint(ctx context.Context, c *Checkpoint) error {
	if err := c.Validate(); err != nil {
		return err
	}
	now := normTime(s.now())
	c.CreatedAt, c.UpdatedAt = now, now
	return mapDBError(s.db.WithContext(ctx).Create(c).Error)
}

// UpdateCheckpoint rewrites every editable field of checkpoint c.ID.
func (s *Store) UpdateCheckpoint(ctx context.Context, c *Checkpoint) error {
	if err := c.Validate(); err != nil {
		return err
	}
	c.UpdatedAt = normTime(s.now())
	res := s.db.WithContext(ctx).Model(&Checkpoint{}).Where("id = ?", c.ID).Updates(map[string]any{
		"code":          c.Code,
		"name":          c.Name,
		"course_order":  c.CourseOrder,
		"expected_call": c.ExpectedCall,
		"updated_at":    c.UpdatedAt,
	})
	return rowsOrNotFound(res)
}

// DeleteCheckpoint removes checkpoint id. Entries reported under its
// code are kept; they're keyed by code, not id.
func (s *Store) DeleteCheckpoint(ctx context.Context, id uint) error {
	return rowsOrNotFound(s.db.WithContext(ctx).Delete(&Checkpoint{}, id))
}

// ListCheckpoints returns checkpoints in course order.
func (s *Store) ListCheckpoints(ctx context.Context) ([]Checkpoint, error) {
	var out []Checkpoint
	err := s.db.WithContext(ctx).Order("course_order ASC, code ASC").Find(&out).Error
	return out, err
}

// UpsertRunners inserts or updates (by bib) every runner, all or
// nothing. Returns the number written.
func (s *Store) UpsertRunners(ctx context.Context, runners []Runner) (int, error) {
	for i, r := range runners {
		if err := r.Validate(); err != nil {
			return 0, fmt.Errorf("runner %d: %w", i+1, err)
		}
	}
	now := normTime(s.now())
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, r := range runners {
			row := Runner{Bib: r.Bib, Category: r.Category, CreatedAt: now, UpdatedAt: now}
			if err := tx.Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "bib"}},
				DoUpdates: clause.AssignmentColumns([]string{"category", "updated_at"}),
			}).Create(&row).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(runners), nil
}

// GetRunner looks a runner up by bib.
func (s *Store) GetRunner(ctx context.Context, bib Bib) (*Runner, error) {
	var r Runner
	if err := s.db.WithContext(ctx).Where("bib = ?", bib).First(&r).Error; err != nil {
		return nil, mapDBError(err)
	}
	return &r, nil
}

// ListRunners returns the roster ordered by bib.
func (s *Store) ListRunners(ctx context.Context) ([]Runner, error) {
	var out []Runner
	err := s.db.WithContext(ctx).Order("bib ASC").Find(&out).Error
	return out, err
}

// DeleteRunner removes a bib from the roster. Its entries are kept and
// show as an unknown bib.
func (s *Store) DeleteRunner(ctx context.Context, bib Bib) error {
	return rowsOrNotFound(s.db.WithContext(ctx).Where("bib = ?", bib).Delete(&Runner{}))
}
