package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
)

const hookTok = "0123456789abcdef0123456789"

func hookReq(t *testing.T, e *env, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/hook/linkcheck", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.TrimSpace(string(b))
}

func TestHookOffByDefault(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	if code, _ := hookReq(t, e, hookTok); code != http.StatusNotFound {
		t.Fatalf("hook without a configured token = %d", code)
	}
}

func TestHookRunsLinkCheck(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) {
		d.HookToken = hookTok
		d.LinkTiming = linkcheck.Timing{Poll: 5 * time.Millisecond, StartWait: time.Second, MaxWait: 2 * time.Second}
	})
	if code, _ := hookReq(t, e, ""); code != http.StatusUnauthorized {
		t.Fatalf("no token = %d", code)
	}
	if code, _ := hookReq(t, e, hookTok+"x"); code != http.StatusUnauthorized {
		t.Fatalf("wrong token = %d", code)
	}
	// The service (here, a stand-in) finishes the requested run.
	go func() {
		for range 200 {
			if c, err := e.st.NextRequestedLinkCheck(context.Background()); err == nil {
				now := time.Now()
				c.State, c.StartedAt, c.FinishedAt, c.Verdict, c.Uplink, c.RoundTrip = store.LinkCheckDone, &now, &now, linkcheck.Pass, 5, 5
				_ = e.st.UpdateLinkCheck(context.Background(), c)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	code, body := hookReq(t, e, hookTok)
	if code != http.StatusOK || !strings.HasPrefix(body, "PASS N0CALL-10 up5/5") {
		t.Fatalf("hook = %d %q", code, body)
	}
	// Refusals still answer 200, with the reason (the on-air reply).
	code, body = hookReq(t, e, hookTok)
	if code != http.StatusOK || !strings.Contains(body, "apart") {
		t.Fatalf("too soon = %d %q", code, body)
	}
}

func TestHookLoopbackOnly(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) { d.HookToken = hookTok })
	h, _ := NewHandler(e.deps)
	req := httptest.NewRequest("POST", "/api/hook/linkcheck", strings.NewReader(`{}`))
	req.RemoteAddr = "10.0.0.7:5555"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+hookTok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote hook = %d", rec.Code)
	}
}

func TestHookErrorTextIsSafe(t *testing.T) {
	if line, known := hookError(errors.New("sqlite: disk I/O error at /var/lib/x")); known || strings.Contains(line, "sqlite") {
		t.Fatalf("unknown error on air: %q", line)
	}
	if line, known := hookError(linkcheck.ErrBusy); !known || line == "" {
		t.Fatalf("busy = %q", line)
	}
}
