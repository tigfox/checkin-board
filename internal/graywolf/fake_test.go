package graywolf

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testUser = "race"
	testPass = "s3cret"
)

// fakeGW is a minimal graywolf stand-in: it enforces the session
// cookie on every route except login, and lets each test register the
// handlers it needs.
type fakeGW struct {
	t      *testing.T
	srv    *httptest.Server
	mux    *http.ServeMux
	logins atomic.Int32

	mu      sync.Mutex
	session string
}

func newFakeGW(t *testing.T) *fakeGW {
	t.Helper()
	f := &fakeGW{t: t, mux: http.NewServeMux()}
	f.mux.HandleFunc("POST /api/auth/login", f.handleLogin)
	f.srv = httptest.NewServer(f)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGW) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/auth/login" && !f.authorized(r) {
		writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	f.mux.ServeHTTP(w, r)
}

func (f *fakeGW) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeTestJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if req.Username != testUser || req.Password != testPass {
		writeTestJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	n := f.logins.Add(1)
	token := "tok-" + time.Now().Format("150405.000000000") + string(rune('a'+n%26))
	f.mu.Lock()
	f.session = token
	f.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: token, Path: "/", HttpOnly: true, Secure: true})
	writeTestJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (f *fakeGW) authorized(r *http.Request) bool {
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.session != "" && c.Value == f.session
}

// expireSession simulates graywolf restarting or the session timing out.
func (f *fakeGW) expireSession() {
	f.mu.Lock()
	f.session = ""
	f.mu.Unlock()
}

func (f *fakeGW) client(t *testing.T) *Client {
	t.Helper()
	c, err := New(Config{BaseURL: f.srv.URL, Username: testUser, Password: testPass, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func writeTestJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
