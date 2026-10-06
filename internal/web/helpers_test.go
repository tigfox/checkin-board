package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"checkin-board/internal/auth"
	"checkin-board/internal/checkpoint"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/hq"
	"checkin-board/internal/inbox"
	"checkin-board/internal/ops"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

const (
	adminPW = "correct horse battery"
	volPW   = "keypad1"
)

type fakeInbox struct {
	mu     sync.Mutex
	status inbox.Status
	kicks  int
}

func (f *fakeInbox) Status() inbox.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.status
}

func (f *fakeInbox) Kick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kicks++
}

type env struct {
	ops    *ops.Service
	hq     *hq.Engine
	t      *testing.T
	srv    *httptest.Server
	st     *store.Store
	gw     *gwfake.Station
	auth   *auth.Service
	cp     *checkpoint.Engine
	inbox  *fakeInbox
	deps   Deps
	admin  string // session tokens
	volunt string
}

// newEnv builds the whole server on the fake graywolf with the given
// settings, an admin and a volunteer password, and a session for each.
func newEnv(t *testing.T, cfg store.Settings) *env {
	t.Helper()
	return newEnvWith(t, cfg, nil)
}

// newEnvWith is newEnv with a chance to adjust the handler's Deps.
func newEnvWith(t *testing.T, cfg store.Settings, adjust func(*Deps)) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "checkin-board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	gw := gwfake.New("K1CP")
	clock := raceclock.NewClock(nil, func() bool { return true })
	ens := peers.NewEnsurer(gw, st, nil)
	cp, _ := checkpoint.New(checkpoint.Config{Store: st, Graywolf: gw, Clock: clock, Peers: ens})
	hqe, _ := hq.New(hq.Config{Store: st, Graywolf: gw, Clock: clock, Peers: ens})
	opsSvc, err := ops.New(ops.Config{Store: st, Graywolf: gw, Clock: clock, Peers: ens, Checkpoint: cp, HQ: hqe,
		JournalPath: filepath.Join(dir, "race-journal.csv"), BackupDir: filepath.Join(dir, "backups")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = opsSvc.Close() })
	a := auth.New(st, nil, bcrypt.MinCost)
	in := &fakeInbox{}
	deps := Deps{Store: st, Auth: a, Ops: opsSvc, HQ: hqe, Checkpoint: cp, Inbox: in, Clock: clock, Graywolf: gw}
	if adjust != nil {
		adjust(&deps)
	}
	h, err := NewHandler(deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	e := &env{t: t, srv: srv, st: st, gw: gw, auth: a, cp: cp, inbox: in, ops: opsSvc, hq: hqe, deps: deps}
	code, _ := a.SetupCode(ctx)
	if e.admin, err = a.Setup(ctx, code, adminPW); err != nil {
		t.Fatal(err)
	}
	if err := a.SetVolunteerPassword(ctx, volPW); err != nil {
		t.Fatal(err)
	}
	if e.volunt, err = a.Login(ctx, store.RoleVolunteer, volPW, "test"); err != nil {
		t.Fatal(err)
	}
	return e
}

func checkpointSettings(state string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceName, c.RaceState = store.RoleCheckpoint, "AS5", "N0HQ", "Ridge 50K", state
	return c
}

func hqSettings(state string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceName, c.RaceState = store.RoleHQ, "START,FIN", "Ridge 50K", state
	return c
}

// do sends a request with an optional session token and JSON body.
func (e *env) do(method, path, token string, body any) *http.Response {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// upload sends a multipart file upload in the "file" field.
func (e *env) upload(method, path, token, name string, content []byte) *http.Response {
	e.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		e.t.Fatal(err)
	}
	_, _ = fw.Write(content)
	_ = mw.Close()
	req, _ := http.NewRequest(method, e.srv.URL+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if token != "" {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func decode[T any](t *testing.T, resp *http.Response) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
	return v
}

func expect(t *testing.T, resp *http.Response, status int) {
	t.Helper()
	if resp.StatusCode != status {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("%s %s = %d, want %d: %s", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, status, b)
	}
}

func mustOps(t *testing.T, e *env) *ops.Service { return e.ops }
func mustHQ(t *testing.T, e *env) *hq.Engine    { return e.hq }
func clockOf() *raceclock.Clock                 { return raceclock.NewClock(nil, nil) }
