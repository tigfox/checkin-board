package graywolf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"missing url", Config{Username: "u", Password: "p"}},
		{"bad scheme", Config{BaseURL: "ftp://x", Username: "u", Password: "p"}},
		{"no host", Config{BaseURL: "http://", Username: "u", Password: "p"}},
		{"missing user", Config{BaseURL: "http://localhost:8080", Password: "p"}},
		{"missing password", Config{BaseURL: "http://localhost:8080", Username: "u"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestLogsInLazilyAndReusesSession(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{Version: "0.14.14", Platform: "linux"})
	})
	c := f.client(t)
	ctx := context.Background()

	for range 3 {
		if _, err := c.Version(ctx); err != nil {
			t.Fatalf("Version: %v", err)
		}
	}
	if got := f.logins.Load(); got != 1 {
		t.Errorf("logins = %d, want 1", got)
	}
}

func TestReloginOnExpiredSession(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{Version: "0.14.14"})
	})
	c := f.client(t)
	ctx := context.Background()

	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}
	f.expireSession()
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if got := f.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2", got)
	}
}

func TestBadCredentialsReturnAuthError(t *testing.T) {
	f := newFakeGW(t)
	c, err := New(Config{BaseURL: f.srv.URL, Username: testUser, Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Version(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("error leaks password: %v", err)
	}
}

func TestPersistent401AfterReloginIsAuthError(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "nope"})
	})
	c := f.client(t)
	_, err := c.Version(context.Background())
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if got := f.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2 (initial + one retry)", got)
	}
}

func TestConcurrentExpiryLogsInOnce(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{Version: "x"})
	})
	c := f.client(t)
	ctx := context.Background()
	if _, err := c.Version(ctx); err != nil {
		t.Fatal(err)
	}
	f.expireSession()

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			if _, err := c.Version(ctx); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent call: %v", err)
	}
	if got := f.logins.Load(); got != 2 {
		t.Errorf("logins = %d, want 2", got)
	}
}

func TestAPIErrorCarriesStatusAndMessage(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
	})
	c := f.client(t)
	_, err := c.GetMessage(context.Background(), 7)

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %v", err)
	}
	if apiErr.StatusCode != http.StatusNotFound || apiErr.Message != "message not found" {
		t.Errorf("got %+v", apiErr)
	}
	if !IsNotFound(err) {
		t.Error("IsNotFound = false")
	}
	if IsConflict(err) {
		t.Error("IsConflict = true")
	}
}

func TestAPIErrorWithNonJSONBody(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream exploded", http.StatusBadGateway)
	})
	c := f.client(t)
	_, err := c.Version(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "upstream exploded" {
		t.Fatalf("got %v", err)
	}
}

func TestUnreachableIsNotAuthError(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", Username: "u", Password: "p", Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Version(context.Background())
	if err == nil || errors.Is(err, ErrAuth) {
		t.Fatalf("want transport error, got %v", err)
	}
}

func TestMalformedJSONResponse(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{nope"))
	})
	c := f.client(t)
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestBadCredentialsBackOff(t *testing.T) {
	f := newFakeGW(t)
	c, err := New(Config{BaseURL: f.srv.URL, Username: testUser, Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for range 5 {
		if _, err := c.Version(ctx); !errors.Is(err, ErrAuth) {
			t.Fatalf("want ErrAuth, got %v", err)
		}
	}
	if got := f.logins.Load(); got != 0 {
		t.Errorf("successful logins = %d, want 0", got)
	}
	// Explicit Login bypasses the backoff (operator fixed the password).
	c.password = testPass
	if err := c.Login(ctx); err != nil {
		t.Fatalf("Login: %v", err)
	}
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{})
	})
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("after Login: %v", err)
	}
}

func TestBadCredentialsRetryAfterBackoff(t *testing.T) {
	f := newFakeGW(t)
	attempts := 0
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{})
	})
	c, err := New(Config{BaseURL: f.srv.URL, Username: testUser, Password: "wrong"})
	if err != nil {
		t.Fatal(err)
	}
	c.authRetry = 50 * time.Millisecond
	ctx := context.Background()
	if _, err := c.Version(ctx); !errors.Is(err, ErrAuth) {
		t.Fatal(err)
	}
	attempts++
	c.password = testPass
	time.Sleep(60 * time.Millisecond)
	if _, err := c.Version(ctx); err != nil {
		t.Fatalf("after backoff (attempt %d): %v", attempts+1, err)
	}
}

func TestBaseURLWithCredentialsRejected(t *testing.T) {
	_, err := New(Config{BaseURL: "http://admin:leak@localhost:8080", Username: "u", Password: "p"})
	if err == nil || strings.Contains(err.Error(), "leak") {
		t.Fatalf("err = %v", err)
	}
}

func TestStringsHidePassword(t *testing.T) {
	cfg := Config{BaseURL: "http://localhost:8080", Username: "u", Password: "hunter2"}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{cfg.String(), fmt.Sprintf("%v", cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), c.String(), cfg.LogValue().String()} {
		if strings.Contains(s, "hunter2") {
			t.Errorf("leaks password: %s", s)
		}
	}
}
