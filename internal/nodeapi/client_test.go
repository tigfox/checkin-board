package nodeapi

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBaseURL(t *testing.T) {
	for in, want := range map[string]string{
		"10.0.1.183":              "http://10.0.1.183:8090",
		" 10.0.1.183 ":            "http://10.0.1.183:8090",
		"10.0.1.183:9000":         "http://10.0.1.183:9000",
		"http://10.0.1.183:8090/": "http://10.0.1.183:8090",
		"https://node.example":    "https://node.example",
		"checkin-board.local":     "http://checkin-board.local:8090",
		"[fe80::1]":               "http://[fe80::1]:8090",
	} {
		if got := BaseURL(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") == "" {
			http.Error(w, "no origin", 400)
			return
		}
		switch r.URL.Path {
		case "/api/login":
			http.SetCookie(w, &http.Cookie{Name: "s", Value: "1"})
		case "/api/me":
			if c, err := r.Cookie("s"); err != nil || c.Value != "1" {
				http.Error(w, `{"error":"login needed"}`, 401)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		case "/api/upload":
			f, h, err := r.FormFile("file")
			if err != nil || h.Filename != "x.json" || r.FormValue("replace") != "1" {
				http.Error(w, "bad upload", 400)
				return
			}
			b, _ := io.ReadAll(f)
			_, _ = w.Write(b)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL)
	var out struct{ OK bool }
	err := c.Do(ctx, "GET", "/api/me", nil, &out)
	if StatusOf(err) != 401 || !strings.Contains(err.Error(), "login needed") {
		t.Fatalf("before login: %v", err)
	}
	if err := c.Login(ctx, "pw"); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "GET", "/api/me", nil, &out); err != nil || !out.OK {
		t.Fatalf("after login: %v %+v", err, out)
	}
	var raw []byte
	if err := c.Upload(ctx, "/api/upload", "x.json", []byte(`{"a":1}`), map[string]string{"replace": "1"}, &raw); err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("upload: %v %q", err, raw)
	}
	if StatusOf(nil) != 0 {
		t.Fatal("StatusOf(nil)")
	}
}

// A redirect is never followed: the login body must not go elsewhere.
func TestNoRedirects(t *testing.T) {
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("followed a redirect")
	}))
	defer elsewhere.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/api/login", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	if err := New(srv.URL).Login(context.Background(), "pw"); StatusOf(err) != http.StatusTemporaryRedirect {
		t.Fatalf("err = %v", err)
	}
}
