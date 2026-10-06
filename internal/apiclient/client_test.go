package apiclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, time.Second)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestNewRejectsBadURL(t *testing.T) {
	for _, raw := range []string{"", "localhost:8080", "://bad"} {
		if _, err := New(raw, time.Second); err == nil {
			t.Errorf("New(%q): expected error", raw)
		}
	}
}

func TestHealth(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/health" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(HealthStatus{Status: "ok"})
	})

	got, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
}

func TestPostSendsJSONAndQuery(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/items" || r.URL.Query().Get("dry") != "1" {
			t.Errorf("unexpected url %s", r.URL)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		var in payload
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Errorf("decode: %v", err)
		}
		_ = json.NewEncoder(w).Encode(in)
	})

	var out payload
	if err := c.Post(context.Background(), "/items?dry=1", payload{Name: "x"}, &out); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if out.Name != "x" {
		t.Errorf("echo = %q, want x", out.Name)
	}
}

func TestNon2xxReturnsAPIError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	})

	err := c.Get(context.Background(), "/anything", nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %v", err)
	}
	if apiErr.StatusCode != http.StatusTeapot || apiErr.Body != "nope" {
		t.Errorf("got %+v", apiErr)
	}
}

func TestEmptyBodyIsNotAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	var out map[string]any
	if err := c.Get(context.Background(), "/empty", &out); err != nil {
		t.Fatalf("Get: %v", err)
	}
}

func TestMalformedJSONIsAnError(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("{not json"))
	})

	var out map[string]any
	if err := c.Get(context.Background(), "/bad", &out); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestContextCancellation(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Get(ctx, "/slow", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
