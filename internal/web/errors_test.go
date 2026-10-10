package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"checkin-board/internal/auth"
	"checkin-board/internal/branding"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/hq"
	"checkin-board/internal/ops"
	"checkin-board/internal/store"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&httpError{418, "teapot", "short and stout"}, 418, "teapot"},
		{&auth.RateLimitError{}, 429, "rate_limited"},
		{auth.ErrBadCredentials, 401, "bad_credentials"},
		{auth.ErrNoSession, 401, "login_required"},
		{auth.ErrNotConfigured, 409, "not_configured"},
		{auth.ErrSetupDone, 409, "setup_done"},
		{auth.ErrBadSetupCode, 403, "bad_setup_code"},
		{auth.ErrWeakPassword, 400, "bad_password"},
		{auth.ErrSamePassword, 400, "bad_password"},
		{store.ErrNotFound, 404, "not_found"},
		{store.ErrAlreadyVoided, 409, "already_voided"},
		{store.ErrConflict, 409, "conflict"},
		{fmt.Errorf("x: %w", store.ErrInvalidInput), 400, "invalid"},
		{store.ErrInvalidSettings, 400, "invalid"},
		{branding.ErrInvalid, 400, "invalid"},
		{branding.ErrBadLogo, 400, "invalid"},
		{ops.ErrConfirmMismatch, 400, "confirm_mismatch"},
		{ops.ErrWrongState, 409, "wrong_state"},
		{ops.ErrWrongRole, 409, "wrong_role"},
		{ops.ErrUnsentData, 409, "unsent_data"},
		{hq.ErrTooSoon, 429, "too_soon"},
		{graywolf.ErrAuth, 502, "graywolf_auth"},
		{&graywolf.APIError{StatusCode: 503, Message: "down"}, 502, "graywolf_error"},
		{errors.New("sql: disk I/O error at /var/lib/secret"), 500, "internal"},
	}
	for _, c := range cases {
		status, code, msg := classify(c.err)
		if status != c.status || code != c.code {
			t.Errorf("classify(%v) = %d %s, want %d %s", c.err, status, code, c.status, c.code)
		}
		if status == 500 && msg != "internal error" {
			t.Errorf("500 leaks detail: %q", msg)
		}
	}
	if (&httpError{message: "m"}).Error() != "m" {
		t.Error("httpError.Error")
	}
}

func TestFirstRunSetupOnFreshNode(t *testing.T) {
	e := newEnv(t, checkpointSettings("setup"))
	// Swap in an auth service over a fresh database: no admin yet.
	fresh, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	*e.auth = *auth.New(fresh, nil, bcrypt.MinCost)
	st := decode[map[string]bool](t, e.do("GET", "/api/setup", "", nil))
	if !st["needs_setup"] {
		t.Fatalf("setup = %v", st)
	}
	expect(t, e.do("POST", "/api/setup", "", map[string]string{"password": "first admin password", "setup_code": "NOPE"}), http.StatusForbidden)
	code, _ := e.auth.SetupCode(ctx)
	resp := e.do("POST", "/api/setup", "", map[string]string{"password": "first admin password", "setup_code": code})
	expect(t, resp, http.StatusCreated)
	if len(resp.Cookies()) == 0 {
		t.Fatal("setup didn't log the admin in")
	}
	// Volunteers can't log in until the admin sets their password.
	expect(t, e.do("POST", "/api/login", "", map[string]string{"role": "volunteer", "password": "anything"}), http.StatusConflict)
}

type downGraywolf struct{}

func (downGraywolf) Version(context.Context) (graywolf.Version, error) {
	return graywolf.Version{}, errors.New("dial tcp: connection refused")
}
func (downGraywolf) AudioDevices(context.Context) ([]graywolf.AudioDevice, error) {
	return nil, errors.New("down")
}
func (downGraywolf) AudioLevels(context.Context) (map[uint32]graywolf.DeviceLevel, error) {
	return nil, errors.New("down")
}
func (downGraywolf) Channels(context.Context) ([]graywolf.Channel, error) {
	return nil, errors.New("down")
}
func (downGraywolf) ChannelStats(context.Context, uint32) (graywolf.ChannelStats, error) {
	return graywolf.ChannelStats{}, errors.New("down")
}
func (downGraywolf) TxTimings(context.Context) ([]graywolf.TxTiming, error) {
	return nil, errors.New("down")
}
func (downGraywolf) SetTxTiming(context.Context, graywolf.TxTiming) (graywolf.TxTiming, error) {
	return graywolf.TxTiming{}, errors.New("down")
}
func (downGraywolf) Digipeater(context.Context) (graywolf.Digipeater, error) {
	return graywolf.Digipeater{}, errors.New("down")
}
func (downGraywolf) SetDigipeater(context.Context, graywolf.Digipeater) (graywolf.Digipeater, error) {
	return graywolf.Digipeater{}, errors.New("down")
}
func (downGraywolf) SetMessagePreferences(context.Context, graywolf.MessagePreferences) (graywolf.MessagePreferences, error) {
	return graywolf.MessagePreferences{}, errors.New("down")
}
func (downGraywolf) StationConfig(context.Context) (graywolf.StationConfig, error) {
	return graywolf.StationConfig{}, errors.New("down")
}
func (downGraywolf) SetStationCallsign(context.Context, string) (graywolf.StationConfig, error) {
	return graywolf.StationConfig{}, &graywolf.APIError{StatusCode: 400, Message: "bad callsign"}
}
func (downGraywolf) MessagePreferences(context.Context) (graywolf.MessagePreferences, error) {
	return graywolf.MessagePreferences{}, errors.New("down")
}

func TestGraywolfPanelWhenUnreachable(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	h, err := NewHandler(Deps{Store: e.st, Auth: e.auth, Ops: mustOps(t, e), HQ: mustHQ(t, e), Checkpoint: e.cp,
		Inbox: e.inbox, Clock: clockOf(), Graywolf: downGraywolf{}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()
	e.srv = srv
	v := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if v.Reachable || len(v.Warnings) == 0 || v.Warnings[0] != "graywolf not reachable: unreachable" {
		t.Fatalf("panel = %+v (must not leak internal error text)", v)
	}
	expect(t, e.do("PUT", "/api/admin/callsign", e.admin, map[string]any{"callsign": "N0C", "confirm": true}), http.StatusBadGateway)
}

func TestUploadWithoutFile(t *testing.T) {
	e := newEnv(t, hqSettings("active"))
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/admin/runners/import", bytes.NewReader([]byte("--x--\r\n")))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.admin})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expect(t, resp, http.StatusBadRequest)
}

func TestRecovererHidesPanics(t *testing.T) {
	s := &server{log: slog.New(slog.DiscardHandler)}
	h := s.recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom at /secret/path") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/x", nil))
	body, _ := io.ReadAll(rec.Body)
	if rec.Code != 500 || bytes.Contains(body, []byte("secret")) {
		t.Fatalf("panic response = %d %s", rec.Code, body)
	}
}

func TestClientKeyGroupsIPv6(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "[2001:db8:1:2:aaaa::1]:5555"
	a := clientKey(r)
	r.RemoteAddr = "[2001:db8:1:2:bbbb::9]:6666"
	if b := clientKey(r); a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("keys = %q, %q", a, b)
	}
	r.RemoteAddr = "192.168.4.20:1234"
	if k := clientKey(r); k != "192.168.4.20" {
		t.Fatalf("v4 key = %q", k)
	}
}
