package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// BrandingRow is the stored status-board branding (spec 8.3). Empty
// fields mean "use the default". Validation lives in package branding.
type BrandingRow struct {
	ID              uint      `gorm:"column:id;primaryKey"`
	HeaderText      string    `gorm:"column:header_text"`
	FooterText      string    `gorm:"column:footer_text"`
	ColorPrimary    string    `gorm:"column:color_primary"`
	ColorAccent     string    `gorm:"column:color_accent"`
	ColorBackground string    `gorm:"column:color_background"`
	ColorText       string    `gorm:"column:color_text"`
	UpdatedAt       time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (BrandingRow) TableName() string { return "branding" }

// LogoRow is the stored, already re-encoded logo.
type LogoRow struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	Image       []byte    `gorm:"column:image"`
	ContentType string    `gorm:"column:content_type"`
	Width       int       `gorm:"column:width"`
	Height      int       `gorm:"column:height"`
	SHA256      string    `gorm:"column:sha256"`
	UpdatedAt   time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (LogoRow) TableName() string { return "branding_logo" }

// Branding returns the stored branding (zero value if never set).
func (s *Store) Branding(ctx context.Context) (BrandingRow, error) {
	var b BrandingRow
	err := s.db.WithContext(ctx).First(&b, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return BrandingRow{}, nil
	}
	b.UpdatedAt = normTime(b.UpdatedAt)
	return b, err
}

// SaveBranding stores branding the caller has validated.
func (s *Store) SaveBranding(ctx context.Context, b BrandingRow) (BrandingRow, error) {
	row := b
	row.ID, row.UpdatedAt = 1, normTime(s.now())
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return BrandingRow{}, err
	}
	return row, nil
}

// Logo returns the stored logo, or ErrNotFound.
func (s *Store) Logo(ctx context.Context) (LogoRow, error) {
	var l LogoRow
	if err := s.db.WithContext(ctx).First(&l, 1).Error; err != nil {
		return LogoRow{}, mapDBError(err)
	}
	l.UpdatedAt = normTime(l.UpdatedAt)
	return l, nil
}

// SaveLogo stores a logo the caller has decoded and re-encoded.
func (s *Store) SaveLogo(ctx context.Context, l LogoRow) error {
	row := l
	row.ID, row.UpdatedAt = 1, normTime(s.now())
	return s.db.WithContext(ctx).Save(&row).Error
}

// DeleteLogo removes the logo (no-op if none).
func (s *Store) DeleteLogo(ctx context.Context) error {
	return s.db.WithContext(ctx).Where("id = 1").Delete(&LogoRow{}).Error
}

// ClearBranding returns the board to the default look.
func (s *Store) ClearBranding(ctx context.Context) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("id = 1").Delete(&BrandingRow{}).Error; err != nil {
			return err
		}
		return tx.Where("id = 1").Delete(&LogoRow{}).Error
	})
}
