package web

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/store"
)

func TestLoginSetupAndLogoutFlow(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	setup := decode[map[string]bool](t, e.do("GET", "/api/setup", "", nil))
	if setup["needs_setup"] || !setup["volunteer_set"] {
		t.Fatalf("setup = %v", setup)
	}
	expect(t, e.do("POST", "/api/setup", "", map[string]string{"password": "another long password"}), http.StatusConflict)

	resp := e.do("POST", "/api/login", "", map[string]string{"role": "volunteer", "password": volPW})
	expect(t, resp, http.StatusOK)
	var tok string
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			tok = c.Value
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				t.Errorf("cookie flags: %+v", c)
			}
		}
	}
	if tok == "" {
		t.Fatal("no session cookie")
	}
	who := decode[map[string]string](t, e.do("GET", "/api/session", tok, nil))
	if who["role"] != "volunteer" {
		t.Fatalf("session = %v", who)
	}
	expect(t, e.do("POST", "/api/login", "", map[string]string{"role": "admin", "password": "nope"}), http.StatusUnauthorized)
	expect(t, e.do("POST", "/api/logout", tok, nil), http.StatusNoContent)
	expect(t, e.do("GET", "/api/session", tok, nil), http.StatusUnauthorized)
}

func TestLoginLockoutReturns429(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	var last *http.Response
	for range 6 {
		last = e.do("POST", "/api/login", "", map[string]string{"role": "admin", "password": "guess"})
	}
	expect(t, last, http.StatusTooManyRequests)
	if last.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After header")
	}
}

// Once set up, setup is refused before any password work (no bcrypt
// cost for strangers).
func TestSetupRefusedOnceDone(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	expect(t, e.do("POST", "/api/setup", "", map[string]string{"password": "whatever long enough"}), http.StatusConflict)
}

func TestKeypadFlow(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	res := decode[entryResult](t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "0042"}))
	if res.ID == 0 || !res.ClockSynced {
		t.Fatalf("entry = %+v", res)
	}
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "0"}), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "7", "cp": "FIN"}), http.StatusBadRequest)

	list := decode[map[string][]map[string]any](t, e.do("GET", "/api/entries?limit=5", e.volunt, nil))
	if len(list["entries"]) != 1 || list["entries"][0]["Bib"].(float64) != 42 {
		t.Fatalf("entries = %v", list)
	}
	expect(t, e.do("GET", "/api/entries?limit=0", e.volunt, nil), http.StatusBadRequest)
	expect(t, e.do("DELETE", "/api/entries/1", e.volunt, nil), http.StatusNoContent)
	expect(t, e.do("DELETE", "/api/entries/1", e.volunt, nil), http.StatusNotFound) // the queued entry was deleted
	expect(t, e.do("DELETE", "/api/entries/abc", e.volunt, nil), http.StatusBadRequest)

	st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil))
	if st.Role != "checkpoint" || st.RaceState != "active" || len(st.LocalCodes) != 1 || !st.Journal.Enabled {
		t.Fatalf("station = %+v", st)
	}
}

func TestKeypadClosedOutsideActive(t *testing.T) {
	e := newEnv(t, checkpointSettings("setup"))
	resp := e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "1"})
	expect(t, resp, http.StatusConflict)
	body := decode[errorBody](t, resp)
	if body.Code != "wrong_state" {
		t.Fatalf("code = %q", body.Code)
	}
}

func TestClockSync(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	now := time.Now()
	v := decode[clockView](t, e.do("POST", "/api/clock/sync", e.volunt, map[string]int64{"client_unix_ms": now.UnixMilli(), "rtt_ms": 40}))
	if v.Source == "" || v.Now.IsZero() {
		t.Fatalf("clock = %+v", v)
	}
	expect(t, e.do("POST", "/api/clock/sync", e.volunt, map[string]int64{"client_unix_ms": 1}), http.StatusBadRequest)
	expect(t, e.do("GET", "/api/clock", e.volunt, nil), http.StatusOK)
}

func TestSettingsEditAndRoleGuard(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	v := decode[settingsView](t, e.do("GET", "/api/admin/settings", e.admin, nil))
	body := v.settingsBody
	body.StationTactical, body.Path = "  Ridge Aid #5 ", "wide2-1"
	got := decode[settingsView](t, e.do("PUT", "/api/admin/settings", e.admin, body))
	if got.StationTactical != "Ridge Aid #5" || got.Path != "WIDE2-1" || got.RaceState != "active" {
		t.Fatalf("saved = %+v", got)
	}
	body.Role, body.HQLocalCodes = "hq", []string{"fin"}
	expect(t, e.do("PUT", "/api/admin/settings", e.admin, body), http.StatusConflict) // mid-race
	bad := v.settingsBody
	bad.HQCall = "not a call"
	expect(t, e.do("PUT", "/api/admin/settings", e.admin, bad), http.StatusBadRequest)
}

// The race and messaging forms each save only their own fields, so one
// can't overwrite the other with stale values.
func TestSettingsSplitEndpoints(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	v := decode[settingsView](t, e.do("GET", "/api/admin/settings", e.admin, nil))

	race := v.raceSettingsBody
	race.StationTactical, race.CheckpointCode = "Ridge (Mile 21)", "as9"
	got := decode[settingsView](t, e.do("PUT", "/api/admin/settings/race", e.admin, race))
	if got.StationTactical != "Ridge (Mile 21)" || got.CheckpointCode != "AS9" || got.messagingSettingsBody != v.messagingSettingsBody {
		t.Fatalf("race save = %+v", got)
	}

	msg := v.messagingSettingsBody // stale race fields must not come back
	msg.HeartbeatSec, msg.Path = 600, "wide1-1"
	got = decode[settingsView](t, e.do("PUT", "/api/admin/settings/messaging", e.admin, msg))
	if got.HeartbeatSec != 600 || got.Path != "WIDE1-1" || got.StationTactical != "Ridge (Mile 21)" || got.CheckpointCode != "AS9" {
		t.Fatalf("messaging save = %+v", got)
	}

	msg.HeartbeatSec = 1
	expect(t, e.do("PUT", "/api/admin/settings/messaging", e.admin, msg), http.StatusBadRequest)
	race.Role = "hq"
	expect(t, e.do("PUT", "/api/admin/settings/race", e.admin, race), http.StatusConflict) // mid-race
}

func TestCallsignNeedsConfirm(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	expect(t, e.do("PUT", "/api/admin/callsign", e.admin, map[string]any{"callsign": "N0CALL-15"}), http.StatusBadRequest)
	got := decode[map[string]string](t, e.do("PUT", "/api/admin/callsign", e.admin, map[string]any{"callsign": "n0call-15", "confirm": true}))
	if got["callsign"] != "N0CALL-15" {
		t.Fatalf("callsign = %v", got)
	}
	gw := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if !gw.Reachable || gw.Callsign != "N0CALL-15" || gw.MaxText != 67 {
		t.Fatalf("gw = %+v", gw)
	}
}

func TestLifecycleOverHTTP(t *testing.T) {
	e := newEnv(t, checkpointSettings("setup"))
	expect(t, e.do("POST", "/api/admin/race/start", e.admin, nil), http.StatusOK)
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "5"}), http.StatusCreated)
	expect(t, e.do("POST", "/api/admin/race/complete", e.admin, nil), http.StatusOK)
	sec := decode[map[string]any](t, e.do("POST", "/api/admin/race/secure", e.admin, nil))
	if sec["unconfirmed"].(float64) != 1 {
		t.Fatalf("secure = %v", sec)
	}
	expect(t, e.do("POST", "/api/admin/race/check-in", e.admin, nil), http.StatusOK)
	expect(t, e.do("POST", "/api/admin/race/cleanup-graywolf", e.admin, nil), http.StatusConflict) // not during check-in
	expect(t, e.do("POST", "/api/admin/race/start", e.admin, nil), http.StatusConflict)

	outbox := decode[map[string]any](t, e.do("GET", "/api/admin/outbox", e.admin, nil))
	if outbox["stats"] == nil {
		t.Fatalf("outbox = %v", outbox)
	}
	exp := e.do("GET", "/api/admin/recovery/export.csv", e.admin, nil)
	expect(t, exp, http.StatusOK)
	raw, _ := io.ReadAll(exp.Body)
	if !strings.HasPrefix(string(raw), "cp,seq,event") || exp.Header.Get("Content-Disposition") == "" {
		t.Fatalf("export = %q", raw)
	}

	expect(t, e.do("POST", "/api/admin/race/reset", e.admin, map[string]any{"confirm": "nope"}), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/admin/race/reset", e.admin, map[string]any{"confirm": "Ridge 50K"}), http.StatusConflict) // unsent
	res := decode[map[string]any](t, e.do("POST", "/api/admin/race/reset", e.admin,
		map[string]any{"confirm": "Ridge 50K", "acknowledge_unsent": true}))
	if res["backup_dir"] == "" {
		t.Fatalf("reset = %v", res)
	}
}

func TestHQToolsOverHTTP(t *testing.T) {
	e := newEnv(t, hqSettings("active"))
	cp := decode[store.Checkpoint](t, e.do("POST", "/api/admin/checkpoints", e.admin,
		map[string]any{"code": "as5", "name": "Aid 5", "course_order": 1, "expected_call": "n0call-1"}))
	if cp.ID == 0 || cp.Code != "AS5" || cp.ExpectedCall != "N0CALL-1" {
		t.Fatalf("checkpoint = %+v", cp)
	}
	expect(t, e.do("POST", "/api/admin/checkpoints", e.admin, map[string]any{"code": "AS5", "name": "dup"}), http.StatusConflict)
	expect(t, e.do("PUT", "/api/admin/checkpoints/1", e.admin, map[string]any{"code": "AS5", "name": "Aid Five", "course_order": 2}), http.StatusOK)
	expect(t, e.do("GET", "/api/admin/checkpoints", e.admin, nil), http.StatusOK)

	expect(t, e.do("POST", "/api/admin/runners", e.admin, map[string]string{"bib": "101", "category": "50K"}), http.StatusNoContent)
	imp := decode[map[string]int](t, e.upload("POST", "/api/admin/runners/import", e.admin, "roster.csv",
		[]byte("bib,name,category\n102,Jane Doe,25K\n103,John Roe,50K\n")))
	if imp["runners"] != 2 {
		t.Fatalf("import = %v", imp)
	}
	rs := decode[map[string][]store.Runner](t, e.do("GET", "/api/admin/runners", e.admin, nil))
	if len(rs["runners"]) != 3 {
		t.Fatalf("runners = %+v", rs)
	}
	bad := e.upload("POST", "/api/admin/runners/import", e.admin, "roster.csv", []byte("bib\n0\n"))
	expect(t, bad, http.StatusBadRequest)

	// A finish-line entry on HQ's own keypad, and the board.
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "101", "cp": "FIN"}), http.StatusCreated)
	board := decode[map[string]any](t, e.do("GET", "/api/admin/board", e.admin, nil))
	if len(board["Runners"].([]any)) != 3 {
		t.Fatalf("board runners = %v", board["Runners"])
	}
	expect(t, e.do("GET", "/api/admin/runners/101/history", e.admin, nil), http.StatusOK)
	expect(t, e.do("GET", "/api/admin/runners/0/history", e.admin, nil), http.StatusBadRequest)
	res := e.do("GET", "/api/admin/export.csv", e.admin, nil)
	expect(t, res, http.StatusOK)
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("export type = %q", ct)
	}
	status := decode[map[string]any](t, e.do("GET", "/api/admin/status", e.admin, nil))
	if len(status["checkpoints"].([]any)) != 1 {
		t.Fatalf("status = %v", status)
	}
	expect(t, e.do("POST", "/api/admin/status/AS5/rerequest", e.admin, nil), http.StatusNotFound) // never heard

	journal := "logged_at,event,cp,bib,time_in,clock_synced\n2026-10-10T13:00:00Z,entry,AS5,103,2026-10-10T13:00:00Z,true\n"
	j := decode[map[string]any](t, e.upload("POST", "/api/admin/recovery/journal", e.admin, "race-journal.csv", []byte(journal)))
	if j["Added"].(float64) != 1 {
		t.Fatalf("journal import = %v", j)
	}
	export := "cp,seq,event,bib,time_in,clock_synced,msg_id\nAS5,1,entry,102,2026-10-10T13:05:00Z,true,9\n"
	ex := decode[map[string]any](t, e.upload("POST", "/api/admin/recovery/import", e.admin, "export.csv", []byte(export)))
	if ex["Entries"].(float64) != 1 {
		t.Fatalf("export import = %v", ex)
	}
	expect(t, e.do("DELETE", "/api/admin/runners/102", e.admin, nil), http.StatusNoContent)
	expect(t, e.do("DELETE", "/api/admin/runners/9999", e.admin, nil), http.StatusNotFound)
	expect(t, e.do("DELETE", "/api/admin/checkpoints/1", e.admin, nil), http.StatusNoContent)
	expect(t, e.do("DELETE", "/api/admin/checkpoints/x", e.admin, nil), http.StatusBadRequest)
}

func TestHQOnlyToolsRefusedOnCheckpoint(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	expect(t, e.do("GET", "/api/admin/board", e.admin, nil), http.StatusConflict)
	expect(t, e.do("GET", "/api/admin/status", e.admin, nil), http.StatusConflict)
	expect(t, e.do("POST", "/api/admin/status/AS5/rerequest", e.admin, nil), http.StatusConflict)
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestBrandingOverHTTP(t *testing.T) {
	e := newEnv(t, hqSettings("active"))
	def := decode[brandingView](t, e.do("GET", "/api/branding", e.volunt, nil))
	if def.HeaderText != "Ridge 50K" || def.HasLogo || def.Colors.Background == "" {
		t.Fatalf("default branding = %+v", def)
	}
	chk := decode[map[string]any](t, e.do("POST", "/api/admin/branding/check", e.admin,
		map[string]any{"colors": map[string]string{"text": "#777777"}}))
	if chk["ok"].(bool) || chk["problems"] == nil || chk["contrasts"] == nil {
		t.Fatalf("check = %v", chk)
	}
	bad := e.do("PUT", "/api/admin/branding", e.admin, map[string]any{"colors": map[string]string{"text": "#777777"}})
	expect(t, bad, http.StatusBadRequest)
	if body := decode[errorBody](t, bad); body.Details == nil {
		t.Fatal("contrast details missing from the error")
	}
	expect(t, e.do("PUT", "/api/admin/branding", e.admin, map[string]any{
		"header_text": "Ridge Run", "footer_text": "Thanks, volunteers", "colors": map[string]string{"primary": "#0b3d91"},
	}), http.StatusOK)

	expect(t, e.upload("PUT", "/api/admin/branding/logo", e.admin, "logo.svg", []byte("<svg/>")), http.StatusBadRequest)
	expect(t, e.upload("PUT", "/api/admin/branding/logo", e.admin, "logo.png", pngBytes(t, 800, 1000)), http.StatusOK)
	b := decode[brandingView](t, e.do("GET", "/api/branding", e.volunt, nil))
	if b.HeaderText != "Ridge Run" || !b.HasLogo || b.LogoHeight != 512 || b.Colors.Primary != "#0B3D91" {
		t.Fatalf("branding = %+v", b)
	}
	logo := e.do("GET", "/api/branding/logo", e.volunt, nil)
	expect(t, logo, http.StatusOK)
	if logo.Header.Get("Content-Type") != "image/png" || logo.Header.Get("ETag") == "" {
		t.Fatalf("logo headers = %v", logo.Header)
	}
	req, _ := http.NewRequest("GET", e.srv.URL+"/api/branding/logo", nil)
	req.Header.Set("If-None-Match", logo.Header.Get("ETag"))
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.volunt})
	cached, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cached.Body.Close()
	if cached.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional GET = %d", cached.StatusCode)
	}
	expect(t, e.do("GET", "/api/admin/branding", e.admin, nil), http.StatusOK)
	expect(t, e.do("DELETE", "/api/admin/branding/logo", e.admin, nil), http.StatusNoContent)
	expect(t, e.do("GET", "/api/branding/logo", e.volunt, nil), http.StatusNotFound)
}

func TestPasswordsOverHTTP(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	expect(t, e.do("PUT", "/api/admin/password/volunteer", e.admin, map[string]string{"new": "abc"}), http.StatusBadRequest)
	expect(t, e.do("PUT", "/api/admin/password/volunteer", e.admin, map[string]string{"new": "newkeypad"}), http.StatusNoContent)
	expect(t, e.do("GET", "/api/station", e.volunt, nil), http.StatusUnauthorized) // volunteers logged out
	expect(t, e.do("PUT", "/api/admin/password/admin", e.admin, map[string]string{"current": "wrong", "new": "a new admin password"}), http.StatusBadRequest)
	expect(t, e.do("PUT", "/api/admin/password/admin", e.admin, map[string]string{"current": adminPW, "new": "a new admin password"}), http.StatusNoContent)
	expect(t, e.do("GET", "/api/admin/settings", e.admin, nil), http.StatusUnauthorized)
}

func TestPeersAndInboxReread(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	expect(t, e.do("GET", "/api/admin/peers", e.admin, nil), http.StatusOK)
	expect(t, e.do("POST", "/api/admin/peers/restore", e.admin, nil), http.StatusNoContent)
	expect(t, e.do("POST", "/api/admin/inbox/reread", e.admin, map[string]any{"since": time.Now().Add(time.Hour)}), http.StatusBadRequest)
	past := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	expect(t, e.do("POST", "/api/admin/inbox/reread", e.admin, map[string]any{"since": past}), http.StatusAccepted)
	if e.inbox.kicks != 1 {
		t.Fatalf("kicks = %d", e.inbox.kicks)
	}
	if since, _ := e.st.InboxSince(ctx); !since.Equal(past) {
		t.Fatalf("since = %v", since)
	}
}

func TestNewHandlerValidates(t *testing.T) {
	if _, err := NewHandler(Deps{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestVolunteerBannerHidesErrorDetail(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	e.inbox.mu.Lock()
	e.inbox.status.StreamError = `graywolf: login: Post "http://192.0.2.65:8080/api/auth/login": refused`
	e.inbox.mu.Unlock()
	resp := e.do("GET", "/api/station", e.volunt, nil)
	raw, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(raw), "192.0.2.65") || !strings.Contains(string(raw), "graywolf unreachable") {
		t.Fatalf("station = %s", raw)
	}
	gw := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if !strings.Contains(gw.StreamError, "192.0.2.65") {
		t.Fatalf("admin panel lacks the detail: %+v", gw)
	}
	e.inbox.mu.Lock()
	e.inbox.status.AuthFailed = true
	e.inbox.mu.Unlock()
	if st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil)); st.Graywolf.Problem != "graywolf rejected the app's login" {
		t.Fatalf("banner = %+v", st.Graywolf)
	}
}

func TestEntryRetryWithRequestIDLogsOnce(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	body := map[string]string{"bib": "77", "request_id": "r-1"}
	first := decode[entryResult](t, e.do("POST", "/api/entries", e.volunt, body))
	again := decode[entryResult](t, e.do("POST", "/api/entries", e.volunt, body))
	if first.ID == 0 || again.ID != first.ID {
		t.Fatalf("retry = %+v, first = %+v", again, first)
	}
	list := decode[map[string][]map[string]any](t, e.do("GET", "/api/entries", e.volunt, nil))
	if len(list["entries"]) != 1 {
		t.Fatalf("entries = %d, want the bib logged once", len(list["entries"]))
	}
	// A failed attempt isn't cached: the same id can succeed later.
	bad := map[string]string{"bib": "0", "request_id": "r-2"}
	expect(t, e.do("POST", "/api/entries", e.volunt, bad), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "78", "request_id": "r-2"}), http.StatusCreated)
	expect(t, e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "1", "request_id": strings.Repeat("x", 65)}), http.StatusBadRequest)
}

func TestRequestDedupExpiresAndBounds(t *testing.T) {
	now := t0
	d := newRequestDedup(func() time.Time { return now })
	release, _, ok := d.begin("a", "77|")
	if !ok {
		t.Fatal("first begin refused")
	}
	if _, prior, ok := d.begin("a", "77|"); ok || prior.status != http.StatusConflict {
		t.Fatal("in-flight duplicate not refused")
	}
	release(201, []byte(`{"id":1}`))
	if _, prior, ok := d.begin("a", "77|"); ok || prior.status != 201 {
		t.Fatalf("stored = %+v", prior)
	}
	// The same id for a different entry is a client bug, not a retry.
	if _, prior, ok := d.begin("a", "78|"); ok || prior.status != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched retry = %+v", prior)
	}
	now = now.Add(dedupTTL)
	if _, _, ok := d.begin("a", "78|"); !ok {
		t.Fatal("expired id still cached")
	}
}

func TestRequestDedupCapHoldsWithLiveEntries(t *testing.T) {
	now := t0
	d := newRequestDedup(func() time.Time { return now })
	for i := range dedupMax * 2 {
		release, _, ok := d.begin(fmt.Sprintf("id-%d", i), "1|")
		if !ok {
			t.Fatalf("begin %d refused", i)
		}
		release(201, []byte(`{}`))
		now = now.Add(time.Millisecond) // all still unexpired
	}
	if n := len(d.entries); n > dedupMax {
		t.Fatalf("entries = %d, cap %d", n, dedupMax)
	}
	// The newest answers are the ones kept.
	if _, prior, ok := d.begin(fmt.Sprintf("id-%d", dedupMax*2-1), "1|"); ok || prior.status != 201 {
		t.Fatal("newest entry evicted")
	}
}

func TestRequestDedupStaleReleaseKeepsNewClaim(t *testing.T) {
	now := t0
	d := newRequestDedup(func() time.Time { return now })
	stale, _, _ := d.begin("a", "1|")
	now = now.Add(dedupTTL) // the first attempt hung past the TTL
	fresh, _, ok := d.begin("a", "1|")
	if !ok {
		t.Fatal("expired claim not replaced")
	}
	stale(500, nil) // must not drop the fresh claim
	if _, prior, ok := d.begin("a", "1|"); ok || prior.status != http.StatusConflict {
		t.Fatalf("fresh claim lost: %+v", prior)
	}
	fresh(201, []byte(`{}`))
}

func TestConcurrentRetriesLogOnce(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	const n = 8
	codes := make(chan int, n)
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			resp := e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "55", "request_id": "same"})
			_ = resp.Body.Close()
			codes <- resp.StatusCode
		})
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusCreated && c != http.StatusConflict {
			t.Fatalf("status %d", c)
		}
	}
	list := decode[map[string][]map[string]any](t, e.do("GET", "/api/entries", e.volunt, nil))
	if len(list["entries"]) != 1 {
		t.Fatalf("entries = %d, want 1", len(list["entries"]))
	}
	resp := e.do("POST", "/api/entries", e.volunt, map[string]string{"bib": "56", "request_id": "same"})
	expect(t, resp, http.StatusUnprocessableEntity)
}
