package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"checkin-board/internal/store"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

type fakeTime struct {
	mu sync.Mutex
	t  time.Time
}

func (f *fakeTime) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.t
}

func (f *fakeTime) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.t = f.t.Add(d)
}

const (
	adminPW = "correct horse battery"
	volPW   = "keypad1"
)

func newSvc(t *testing.T) (*Service, *fakeTime) {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ft := &fakeTime{t: t0}
	st.SetClock(ft.Now)
	return New(st, ft.Now, bcrypt.MinCost), ft
}

func setUp(t *testing.T) (*Service, *fakeTime) {
	t.Helper()
	s, ft := newSvc(t)
	code, _ := s.SetupCode(ctx)
	if _, err := s.Setup(ctx, code, adminPW); err != nil {
		t.Fatal(err)
	}
	if err := s.SetVolunteerPassword(ctx, volPW); err != nil {
		t.Fatal(err)
	}
	return s, ft
}

func TestFirstRunSetup(t *testing.T) {
	s, _ := newSvc(t)
	st, _ := s.Status(ctx)
	if !st.NeedsSetup || st.VolunteerSet {
		t.Fatalf("status = %+v", st)
	}
	code, err := s.SetupCode(ctx)
	if err != nil || len(code) != 9 {
		t.Fatalf("setup code = %q, %v", code, err)
	}
	if again, _ := s.SetupCode(ctx); again != code {
		t.Fatal("setup code changed between calls")
	}
	if _, err := s.Setup(ctx, "WRONG-CODE", adminPW); !errors.Is(err, ErrBadSetupCode) {
		t.Fatalf("wrong code err = %v", err)
	}
	if _, err := s.Setup(ctx, code, "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak err = %v", err)
	}
	tok, err := s.Setup(ctx, strings.ToLower(code), adminPW)
	if err != nil {
		t.Fatal(err)
	}
	if role, err := s.Authenticate(ctx, tok); err != nil || role != store.RoleAdmin {
		t.Fatalf("setup session = %q, %v", role, err)
	}
	if _, err := s.Setup(ctx, code, "another long password"); !errors.Is(err, ErrSetupDone) {
		t.Fatalf("second setup err = %v", err)
	}
	if c, _ := s.SetupCode(ctx); c != "" {
		t.Fatalf("setup code after setup = %q", c)
	}
	// Volunteers can't log in until the admin sets their password.
	if _, err := s.Login(ctx, store.RoleVolunteer, "anything", "1.2.3.4"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("volunteer before set err = %v", err)
	}
}

func TestLoginRoles(t *testing.T) {
	s, _ := setUp(t)
	vt, err := s.Login(ctx, store.RoleVolunteer, volPW, "a")
	if err != nil {
		t.Fatal(err)
	}
	if role, _ := s.Authenticate(ctx, vt); role != store.RoleVolunteer {
		t.Fatalf("role = %q", role)
	}
	// The volunteer password doesn't open the admin role, and vice versa.
	if _, err := s.Login(ctx, store.RoleAdmin, volPW, "a"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("volunteer pw as admin err = %v", err)
	}
	if _, err := s.Login(ctx, store.RoleVolunteer, adminPW, "a"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("admin pw as volunteer err = %v", err)
	}
	if _, err := s.Login(ctx, "root", adminPW, "a"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("bad role err = %v", err)
	}
	if _, err := s.Login(ctx, store.RoleAdmin, strings.Repeat("x", 100), "a"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("over-long err = %v", err)
	}
	if err := s.Logout(ctx, vt); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, vt); !errors.Is(err, ErrNoSession) {
		t.Fatalf("after logout err = %v", err)
	}
	if _, err := s.Authenticate(ctx, ""); !errors.Is(err, ErrNoSession) {
		t.Fatal("empty token accepted")
	}
	if _, err := s.Authenticate(ctx, "forged"); !errors.Is(err, ErrNoSession) {
		t.Fatal("forged token accepted")
	}
	if err := s.Logout(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, ft := setUp(t)
	vt, _ := s.Login(ctx, store.RoleVolunteer, volPW, "a")
	at, _ := s.Login(ctx, store.RoleAdmin, adminPW, "a")

	// Admin: idle expiry slides with use, up to the 24 h cap.
	for range 4 {
		ft.Advance(5 * time.Hour)
		if _, err := s.Authenticate(ctx, at); err != nil {
			t.Fatalf("active admin logged out after %v: %v", ft.Now().Sub(t0), err)
		}
	}
	ft.Advance(5 * time.Hour) // 25 h: past the cap even though active
	if _, err := s.Authenticate(ctx, at); !errors.Is(err, ErrNoSession) {
		t.Fatalf("admin past 24 h err = %v", err)
	}
	// Volunteer: fixed 24 h (already past).
	if _, err := s.Authenticate(ctx, vt); !errors.Is(err, ErrNoSession) {
		t.Fatalf("volunteer after 25 h err = %v", err)
	}

	at2, _ := s.Login(ctx, store.RoleAdmin, adminPW, "a")
	ft.Advance(AdminIdle) // idle
	if _, err := s.Authenticate(ctx, at2); !errors.Is(err, ErrNoSession) {
		t.Fatalf("idle admin err = %v", err)
	}
	if n, err := s.PurgeExpired(ctx); err != nil || n < 0 {
		t.Fatal(n, err)
	}
}

func TestPasswordChangesEndSessions(t *testing.T) {
	s, _ := setUp(t)
	vt, _ := s.Login(ctx, store.RoleVolunteer, volPW, "a")
	at, _ := s.Login(ctx, store.RoleAdmin, adminPW, "a")

	if err := s.SetVolunteerPassword(ctx, "newkeypad99"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, vt); !errors.Is(err, ErrNoSession) {
		t.Fatal("volunteer session survived a volunteer password change")
	}
	if _, err := s.Authenticate(ctx, at); err != nil {
		t.Fatal("admin session ended by a volunteer password change")
	}

	if err := s.ChangeAdminPassword(ctx, "wrong current", "a brand new admin pw"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong current err = %v", err)
	}
	if err := s.ChangeAdminPassword(ctx, adminPW, "newkeypad99"); !errors.Is(err, ErrSamePassword) {
		t.Fatalf("same as volunteer err = %v", err)
	}
	if err := s.SetVolunteerPassword(ctx, adminPW); !errors.Is(err, ErrSamePassword) {
		t.Fatalf("volunteer = admin err = %v", err)
	}
	if err := s.SetVolunteerPassword(ctx, "abc"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("short volunteer err = %v", err)
	}
	if err := s.ChangeAdminPassword(ctx, adminPW, "a brand new admin pw"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, at); !errors.Is(err, ErrNoSession) {
		t.Fatal("admin session survived an admin password change")
	}
	if err := s.ResetAdminPassword(ctx, "recovered admin password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Login(ctx, store.RoleAdmin, "recovered admin password", "a"); err != nil {
		t.Fatalf("login after reset: %v", err)
	}
}

func TestLoginRateLimited(t *testing.T) {
	s, ft := setUp(t)
	for range maxFailures {
		if _, err := s.Login(ctx, store.RoleAdmin, "guess", "10.0.0.9"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("err = %v", err)
		}
	}
	// Locked out, even with the right password.
	_, err := s.Login(ctx, store.RoleAdmin, adminPW, "10.0.0.9")
	var rl *RateLimitError
	if !errors.As(err, &rl) || !errors.Is(err, ErrRateLimited) || rl.RetryAfter <= 0 {
		t.Fatalf("err = %v, want a rate limit", err)
	}
	// Other clients and the other role aren't affected.
	if _, err := s.Login(ctx, store.RoleAdmin, adminPW, "10.0.0.10"); err != nil {
		t.Fatalf("other client: %v", err)
	}
	if _, err := s.Login(ctx, store.RoleVolunteer, volPW, "10.0.0.9"); err != nil {
		t.Fatalf("other role: %v", err)
	}
	ft.Advance(firstLockout)
	if _, err := s.Login(ctx, store.RoleAdmin, adminPW, "10.0.0.9"); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
}

func TestLimiterBackoffDoublesAndCaps(t *testing.T) {
	ft := &fakeTime{t: t0}
	l := NewLimiter(ft.Now)
	var lockouts []time.Duration
	for range 6 {
		var rl *RateLimitError
		for range maxFailures + 1 {
			if err := l.Attempt("k"); err != nil {
				errors.As(err, &rl)
			}
		}
		if rl == nil {
			t.Fatal("not locked")
		}
		lockouts = append(lockouts, rl.RetryAfter)
		ft.Advance(rl.RetryAfter)
	}
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i := range want {
		if lockouts[i] != want[i] {
			t.Fatalf("lockouts = %v, want %v", lockouts, want)
		}
	}
	// Attempts spread beyond the window don't lock.
	for range maxFailures + 2 {
		if err := l.Attempt("s"); err != nil {
			t.Fatalf("spread attempts locked: %v", err)
		}
		ft.Advance(window)
	}
	if !strings.Contains((&RateLimitError{RetryAfter: 90 * time.Second}).Error(), "1m30s") {
		t.Fatal("error text")
	}
}

func TestLimiterCountsParallelAttempts(t *testing.T) {
	ft := &fakeTime{t: t0}
	l := NewLimiter(ft.Now)
	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0
	for range 50 {
		wg.Go(func() {
			if l.Attempt("p") == nil {
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed != maxFailures {
		t.Fatalf("parallel guesses allowed = %d, want %d", allowed, maxFailures)
	}
}

func TestLimiterBoundsMemoryAndFailsClosed(t *testing.T) {
	ft := &fakeTime{t: t0}
	l := NewLimiter(ft.Now)
	for i := range maxKeys {
		_ = l.Attempt(string(rune(i)) + "|admin")
	}
	if len(l.keys) > maxKeys {
		t.Fatalf("keys = %d", len(l.keys))
	}
	// Full of recent attempts: a new client is refused, nothing forgotten.
	if err := l.Attempt("new|admin"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("new client err = %v", err)
	}
	ft.Advance(window)
	if err := l.Attempt("new|admin"); err != nil {
		t.Fatalf("after the window: %v", err)
	}
}
