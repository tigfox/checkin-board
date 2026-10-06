// Package auth is the app's own login (spec 7.2): a shared volunteer
// password for the keypad, a separate admin password for everything
// else, server-side sessions, and brute-force limiting. It is unrelated
// to graywolf's login, which the app uses as a client.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"

	"checkin-board/internal/store"
)

// Policy.
const (
	VolunteerTTL  = 24 * time.Hour // a race day
	AdminIdle     = 8 * time.Hour
	AdminMaxAge   = 24 * time.Hour
	MinAdminLen   = 10
	MinVolunteer  = 6
	MaxPassword   = 72 // bcrypt's limit
	touchInterval = time.Minute
)

var (
	// ErrBadCredentials: wrong password (or role).
	ErrBadCredentials = errors.New("auth: wrong password")
	// ErrNotConfigured: that role's password hasn't been set yet.
	ErrNotConfigured = errors.New("auth: not set up yet")
	// ErrSetupDone: first-run setup was already completed.
	ErrSetupDone = errors.New("auth: setup already done")
	// ErrNoSession: missing, unknown or expired session.
	ErrNoSession = errors.New("auth: not logged in")
	// ErrWeakPassword: a new password breaks the rules.
	ErrWeakPassword = errors.New("auth: password does not meet the rules")
	// ErrSamePassword: admin and volunteer passwords must differ.
	ErrSamePassword = errors.New("auth: admin and volunteer passwords must differ")
)

// Store is the persistence auth needs.
type Store interface {
	PasswordHashes(ctx context.Context) (store.PasswordHashes, error)
	SetPasswordHash(ctx context.Context, role, hash string) error
	SetAdminHashIfUnset(ctx context.Context, hash string) error
	CreateSession(ctx context.Context, sess store.Session) error
	GetSession(ctx context.Context, tokenHash string) (store.Session, error)
	TouchSession(ctx context.Context, tokenHash string, seen, expires time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	PurgeExpiredSessions(ctx context.Context, now time.Time) (int, error)
}

// Service implements logins and sessions.
type Service struct {
	store   Store
	now     func() time.Time
	cost    int
	limiter *Limiter
}

// New returns a Service. now nil means time.Now; cost 0 means bcrypt's
// default (tests pass bcrypt.MinCost).
func New(st Store, now func() time.Time, cost int) *Service {
	if now == nil {
		now = time.Now
	}
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	return &Service{store: st, now: now, cost: cost, limiter: NewLimiter(now)}
}

// Status is what the login page needs to know.
type Status struct {
	NeedsSetup   bool // no admin password yet: show first-run setup
	VolunteerSet bool // volunteers can log in
}

// Status reports whether setup is needed.
func (s *Service) Status(ctx context.Context) (Status, error) {
	h, err := s.store.PasswordHashes(ctx)
	if err != nil {
		return Status{}, err
	}
	return Status{NeedsSetup: h.AdminHash == "", VolunteerSet: h.VolunteerHash != ""}, nil
}

// Setup sets the first admin password and logs the admin in. It works
// only while no admin password exists.
func (s *Service) Setup(ctx context.Context, password string) (string, error) {
	if err := checkPassword(password, MinAdminLen); err != nil {
		return "", err
	}
	hash, err := s.hash(password)
	if err != nil {
		return "", err
	}
	if err := s.store.SetAdminHashIfUnset(ctx, hash); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return "", ErrSetupDone
		}
		return "", err
	}
	return s.newSession(ctx, store.RoleAdmin)
}

// Login checks role's password for a client (e.g. its IP) and returns a
// session token. Repeated failures are rate-limited (ErrRateLimited).
func (s *Service) Login(ctx context.Context, role, password, client string) (string, error) {
	if role != store.RoleAdmin && role != store.RoleVolunteer {
		return "", ErrBadCredentials
	}
	key := client + "|" + role
	if err := s.limiter.Allow(key); err != nil {
		return "", err
	}
	h, err := s.store.PasswordHashes(ctx)
	if err != nil {
		return "", err
	}
	hash := h.AdminHash
	if role == store.RoleVolunteer {
		hash = h.VolunteerHash
	}
	if hash == "" {
		return "", ErrNotConfigured
	}
	if len(password) > MaxPassword || bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.limiter.Fail(key)
		return "", ErrBadCredentials
	}
	s.limiter.Succeed(key)
	return s.newSession(ctx, role)
}

// Authenticate resolves a session token to its role, sliding an admin
// session's idle expiry. Expired sessions are deleted.
func (s *Service) Authenticate(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrNoSession
	}
	th := hashToken(token)
	sess, err := s.store.GetSession(ctx, th)
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNoSession
	}
	if err != nil {
		return "", err
	}
	now := s.now()
	if !now.Before(sess.ExpiresAt) {
		_ = s.store.DeleteSession(ctx, th)
		return "", ErrNoSession
	}
	if sess.Role == store.RoleAdmin && now.Sub(sess.LastSeenAt) >= touchInterval {
		expires := minTime(now.Add(AdminIdle), sess.CreatedAt.Add(AdminMaxAge))
		if err := s.store.TouchSession(ctx, th, now, expires); err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	return sess.Role, nil
}

// Logout ends a session. Unknown tokens are ignored.
func (s *Service) Logout(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	return s.store.DeleteSession(ctx, hashToken(token))
}

// ChangeAdminPassword replaces the admin password after checking the
// current one. Every admin session ends, so the caller logs in again.
func (s *Service) ChangeAdminPassword(ctx context.Context, current, next string) error {
	h, err := s.store.PasswordHashes(ctx)
	if err != nil {
		return err
	}
	if h.AdminHash == "" || bcrypt.CompareHashAndPassword([]byte(h.AdminHash), []byte(current)) != nil {
		return ErrBadCredentials
	}
	return s.setPassword(ctx, store.RoleAdmin, next, h.VolunteerHash)
}

// SetVolunteerPassword sets or changes the shared volunteer password;
// every volunteer session ends.
func (s *Service) SetVolunteerPassword(ctx context.Context, next string) error {
	h, err := s.store.PasswordHashes(ctx)
	if err != nil {
		return err
	}
	return s.setPassword(ctx, store.RoleVolunteer, next, h.AdminHash)
}

// ResetAdminPassword sets the admin password without the current one,
// for the host-only `reset-admin-password` command. Race data is
// untouched; every admin session ends.
func (s *Service) ResetAdminPassword(ctx context.Context, next string) error {
	h, err := s.store.PasswordHashes(ctx)
	if err != nil {
		return err
	}
	return s.setPassword(ctx, store.RoleAdmin, next, h.VolunteerHash)
}

// PurgeExpired deletes expired sessions (housekeeping).
func (s *Service) PurgeExpired(ctx context.Context) (int, error) {
	return s.store.PurgeExpiredSessions(ctx, s.now())
}

func (s *Service) setPassword(ctx context.Context, role, next, otherHash string) error {
	minLen := MinVolunteer
	if role == store.RoleAdmin {
		minLen = MinAdminLen
	}
	if err := checkPassword(next, minLen); err != nil {
		return err
	}
	if otherHash != "" && bcrypt.CompareHashAndPassword([]byte(otherHash), []byte(next)) == nil {
		return ErrSamePassword
	}
	hash, err := s.hash(next)
	if err != nil {
		return err
	}
	return s.store.SetPasswordHash(ctx, role, hash)
}

func checkPassword(p string, minLen int) error {
	n := utf8.RuneCountInString(p)
	if n < minLen || len(p) > MaxPassword {
		return fmt.Errorf("%w: %d to %d bytes, at least %d characters", ErrWeakPassword, minLen, MaxPassword, minLen)
	}
	return nil
}

func (s *Service) hash(p string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(p), s.cost)
	if err != nil {
		return "", fmt.Errorf("auth: hash: %w", err)
	}
	return string(h), nil
}

func (s *Service) newSession(ctx context.Context, role string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("auth: token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := s.now()
	ttl := VolunteerTTL
	if role == store.RoleAdmin {
		ttl = AdminIdle
	}
	err := s.store.CreateSession(ctx, store.Session{
		TokenHash: hashToken(token), Role: role, CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(ttl),
	})
	if err != nil {
		return "", err
	}
	return token, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
