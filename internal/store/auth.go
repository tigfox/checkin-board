package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Login roles (spec 7.2).
const (
	RoleAdmin     = "admin"
	RoleVolunteer = "volunteer"
)

// PasswordHashes are the stored bcrypt hashes ("" = not set).
type PasswordHashes struct {
	ID            uint      `gorm:"column:id;primaryKey"`
	AdminHash     string    `gorm:"column:admin_hash"`
	VolunteerHash string    `gorm:"column:volunteer_hash"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (PasswordHashes) TableName() string { return "auth" }

// Session is one login. Only a hash of the token is stored.
type Session struct {
	TokenHash  string    `gorm:"column:token_hash;primaryKey"`
	Role       string    `gorm:"column:role"`
	CreatedAt  time.Time `gorm:"column:created_at;autoCreateTime:false"`
	LastSeenAt time.Time `gorm:"column:last_seen_at"`
	ExpiresAt  time.Time `gorm:"column:expires_at"`
}

func (Session) TableName() string { return "sessions" }

func validLoginRole(role string) bool { return role == RoleAdmin || role == RoleVolunteer }

// PasswordHashes returns the stored hashes (empty if never set).
func (s *Store) PasswordHashes(ctx context.Context) (PasswordHashes, error) {
	var h PasswordHashes
	err := s.db.WithContext(ctx).First(&h, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return PasswordHashes{}, nil
	}
	return h, err
}

// SetPasswordHash stores role's password hash and ends every session of
// that role, in one transaction (spec 7.2: a password change logs out
// everyone using it).
func (s *Store) SetPasswordHash(ctx context.Context, role, hash string) error {
	if !validLoginRole(role) || hash == "" {
		return fmt.Errorf("%w: role %q / empty hash", ErrInvalidInput, role)
	}
	col := role + "_hash"
	now := normTime(s.now())
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&PasswordHashes{ID: 1, UpdatedAt: now}).Error; err != nil {
			return err
		}
		if err := tx.Model(&PasswordHashes{}).Where("id = 1").
			Updates(map[string]any{col: hash, "updated_at": now}).Error; err != nil {
			return err
		}
		return tx.Where("role = ?", role).Delete(&Session{}).Error
	})
}

// SetAdminHashIfUnset stores the first admin password; it fails with
// ErrConflict if one already exists (first-run setup can't be replayed).
func (s *Store) SetAdminHashIfUnset(ctx context.Context, hash string) error {
	if hash == "" {
		return fmt.Errorf("%w: empty hash", ErrInvalidInput)
	}
	now := normTime(s.now())
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&PasswordHashes{ID: 1, UpdatedAt: now}).Error; err != nil {
			return err
		}
		res := tx.Model(&PasswordHashes{}).Where("id = 1 AND admin_hash = ''").
			Updates(map[string]any{"admin_hash": hash, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrConflict
		}
		return nil
	})
}

// CreateSession stores a session.
func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	if !validLoginRole(sess.Role) || sess.TokenHash == "" {
		return fmt.Errorf("%w: session", ErrInvalidInput)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = normTime(sess.CreatedAt), normTime(sess.LastSeenAt), normTime(sess.ExpiresAt)
	return mapDBError(s.db.WithContext(ctx).Create(&sess).Error)
}

// GetSession loads a session by token hash (ErrNotFound if absent).
func (s *Store) GetSession(ctx context.Context, tokenHash string) (Session, error) {
	var sess Session
	if err := s.db.WithContext(ctx).Where("token_hash = ?", tokenHash).First(&sess).Error; err != nil {
		return Session{}, mapDBError(err)
	}
	sess.CreatedAt, sess.LastSeenAt, sess.ExpiresAt = normTime(sess.CreatedAt), normTime(sess.LastSeenAt), normTime(sess.ExpiresAt)
	return sess, nil
}

// TouchSession records use of a session and its new expiry.
func (s *Store) TouchSession(ctx context.Context, tokenHash string, seen, expires time.Time) error {
	return rowsOrNotFound(s.db.WithContext(ctx).Model(&Session{}).Where("token_hash = ?", tokenHash).
		Updates(map[string]any{"last_seen_at": normTime(seen), "expires_at": normTime(expires)}))
}

// DeleteSession ends one session (logout). Unknown tokens are a no-op.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	return s.db.WithContext(ctx).Where("token_hash = ?", tokenHash).Delete(&Session{}).Error
}

// PurgeExpiredSessions deletes sessions that expired before now.
func (s *Store) PurgeExpiredSessions(ctx context.Context, now time.Time) (int, error) {
	res := s.db.WithContext(ctx).Where("expires_at < ?", normTime(now)).Delete(&Session{})
	return int(res.RowsAffected), res.Error
}
