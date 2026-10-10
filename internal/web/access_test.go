package web

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// concretePath fills a route pattern's wildcards with plausible values.
func concretePath(pattern string) string {
	r := strings.NewReplacer("{id}", "1", "{cp}", "AS5", "{bib}", "101")
	return r.Replace(pattern)
}

// TestRouteAccessMatrix walks every route (spec 7.2): with no session,
// with a volunteer session and with an admin session. Protected routes
// must answer 401 without a session; admin routes must answer 403 to a
// volunteer; allowed callers must get past the role check.
func TestRouteAccessMatrix(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	s := &server{}
	for _, rt := range s.routes() {
		path := concretePath(rt.pattern)
		e.freshSessions() // logout and password routes end sessions
		if rt.access == hook {
			// Token-only, and off unless a token is configured.
			for _, tok := range []string{"", e.volunt, e.admin} {
				if code := e.request(rt, path, tok).StatusCode; code != http.StatusNotFound {
					t.Errorf("%s %s with a session = %d, want 404", rt.method, path, code)
				}
			}
			continue
		}
		for _, who := range []struct {
			name, token string
			allowed     bool
		}{
			{"nobody", "", rt.access == public},
			{"volunteer", e.volunt, rt.access != admin},
			{"admin", e.admin, true},
		} {
			name := rt.method + " " + path + " as " + who.name
			// A page sends a browser without a session to the login page.
			if rt.page && who.token == "" {
				if code, loc := e.firstResponse(rt.method, path); code != http.StatusSeeOther || loc != "/login.html" {
					t.Errorf("%s = %d to %q, want 303 to /login.html", name, code, loc)
				}
				continue
			}
			resp := e.request(rt, path, who.token)
			code := resp.StatusCode
			if who.allowed {
				if code == http.StatusUnauthorized && rt.pattern != "/api/login" || code == http.StatusForbidden {
					t.Errorf("%s = %d; caller should be allowed", name, code)
				}
				continue
			}
			want := http.StatusUnauthorized
			if who.token != "" {
				want = http.StatusForbidden
			}
			if code != want {
				t.Errorf("%s = %d, want %d", name, code, want)
			}
		}
	}
}

// request sends a minimal well-formed request for rt.
func (e *env) request(rt route, path, token string) *http.Response {
	if rt.upload {
		return e.upload(rt.method, path, token, "x.csv", []byte("x"))
	}
	var body any
	if rt.method != http.MethodGet && rt.method != http.MethodDelete {
		body = map[string]any{}
	}
	return e.do(rt.method, path, token, body)
}

func TestCrossSiteRequestsRefused(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	// A form post (what a cross-site page can send) is refused even with
	// a valid session.
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/entries", strings.NewReader("bib=101"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.volunt})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expect(t, resp, http.StatusForbidden)

	// A JSON request from another origin is refused too.
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/api/entries", bytes.NewReader([]byte(`{"bib":"101"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.volunt})
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	expect(t, resp2, http.StatusForbidden)

	// Same origin is fine.
	req, _ = http.NewRequest(http.MethodPost, e.srv.URL+"/api/entries", bytes.NewReader([]byte(`{"bib":"101"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", e.srv.URL)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.volunt})
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	expect(t, resp3, http.StatusCreated)
}

func TestSecurityHeadersAndStaticShell(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	resp := e.do(http.MethodGet, "/", "", nil)
	expect(t, resp, http.StatusOK)
	for h, want := range map[string]string{
		"X-Content-Type-Options": "nosniff", "X-Frame-Options": "DENY", "Referrer-Policy": "no-referrer",
	} {
		if got := resp.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
	if !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Error("CSP missing frame-ancestors")
	}
	api := e.do(http.MethodGet, "/api/setup", "", nil)
	if api.Header.Get("Cache-Control") != "no-store" {
		t.Error("API responses must not be cached")
	}
}

func TestOversizedJSONRefused(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	big := map[string]string{"bib": strings.Repeat("9", maxJSONBody+10)}
	resp := e.do(http.MethodPost, "/api/entries", e.volunt, big)
	expect(t, resp, http.StatusRequestEntityTooLarge)
}

func TestUnknownJSONFieldsRefused(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	resp := e.do(http.MethodPost, "/api/entries", e.volunt, map[string]any{"bib": "1", "admin": true})
	expect(t, resp, http.StatusBadRequest)
}

func (e *env) freshSessions() {
	e.t.Helper()
	var err error
	if e.admin, err = e.auth.Login(ctx, "admin", adminPW, "test"); err != nil {
		e.t.Fatal(err)
	}
	if e.volunt, err = e.auth.Login(ctx, "volunteer", volPW, "test"); err != nil {
		e.t.Fatal(err)
	}
}

func TestSecFetchSiteCrossSiteRefused(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/api/logout", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expect(t, resp, http.StatusForbidden)
}

// firstResponse sends an unauthenticated request without following
// redirects and returns its status and Location.
func (e *env) firstResponse(method, path string) (int, string) {
	e.t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequest(method, e.srv.URL+path, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}
