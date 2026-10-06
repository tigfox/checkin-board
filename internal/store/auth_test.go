package store

import (
	"errors"
	"testing"
	"time"
)

func TestPasswordHashesAndSessionRevocation(t *testing.T) {
	s := newTestStore(t)
	if h, err := s.PasswordHashes(ctx); err != nil || h.AdminHash != "" {
		t.Fatalf("fresh = %+v, %v", h, err)
	}
	if err := s.SetAdminHashIfUnset(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdminHashIfUnset(ctx, "a2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second setup err = %v", err)
	}
	for _, sess := range []Session{
		{TokenHash: "v1", Role: RoleVolunteer, CreatedAt: t0, LastSeenAt: t0, ExpiresAt: at(time.Hour)},
		{TokenHash: "v2", Role: RoleVolunteer, CreatedAt: t0, LastSeenAt: t0, ExpiresAt: at(time.Hour)},
		{TokenHash: "ad", Role: RoleAdmin, CreatedAt: t0, LastSeenAt: t0, ExpiresAt: at(time.Hour)},
	} {
		if err := s.CreateSession(ctx, sess); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetPasswordHash(ctx, RoleVolunteer, "v-new"); err != nil {
		t.Fatal(err)
	}
	h, _ := s.PasswordHashes(ctx)
	if h.AdminHash != "a1" || h.VolunteerHash != "v-new" {
		t.Fatalf("hashes = %+v", h)
	}
	if _, err := s.GetSession(ctx, "v1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("volunteer session survived the volunteer password change")
	}
	if _, err := s.GetSession(ctx, "ad"); err != nil {
		t.Fatal("admin session ended by a volunteer password change")
	}
	if err := s.SetPasswordHash(ctx, "root", "x"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad role err = %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := newTestStore(t)
	sess := Session{TokenHash: "t", Role: RoleAdmin, CreatedAt: t0, LastSeenAt: t0, ExpiresAt: at(time.Hour)}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateSession(ctx, Session{TokenHash: "x", Role: "root"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad role err = %v", err)
	}
	if err := s.TouchSession(ctx, "t", at(time.Minute), at(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSession(ctx, "t")
	if err != nil || !got.ExpiresAt.Equal(at(2*time.Hour)) || !got.LastSeenAt.Equal(at(time.Minute)) {
		t.Fatalf("session = %+v, %v", got, err)
	}
	if err := s.TouchSession(ctx, "gone", t0, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("touch unknown err = %v", err)
	}
	if n, _ := s.PurgeExpiredSessions(ctx, at(3*time.Hour)); n != 1 {
		t.Fatalf("purged %d", n)
	}
	if err := s.DeleteSession(ctx, "t"); err != nil {
		t.Fatal(err)
	}
}
