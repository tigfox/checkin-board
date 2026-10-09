package web

import (
	"bytes"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/panel"
	"checkin-board/internal/panel/menu"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

func hookDo(t *testing.T, e *env, method, path string, body any) (int, []byte) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+hookTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func panelEnv(t *testing.T, cfg store.Settings) *env {
	return newEnvWith(t, cfg, func(d *Deps) { d.HookToken = hookTok; d.WebPort = 8090 })
}

func hookView(t *testing.T, e *env) panel.View {
	t.Helper()
	code, raw := hookDo(t, e, "GET", "/api/hook/panel", nil)
	if code != http.StatusOK {
		t.Fatalf("view = %d %s", code, raw)
	}
	var v panel.View
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPanelSettingsAndMenuAdmin(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	got := decode[panelSettingsView](t, e.do("GET", "/api/admin/panel", e.admin, nil))
	if !got.Enabled || got.RefreshMin != 5 || len(got.Controllers) != 3 {
		t.Fatalf("settings = %+v", got)
	}
	expect(t, e.do("PUT", "/api/admin/panel", e.admin, map[string]any{"enabled": true, "refresh_min": 1}), http.StatusBadRequest)
	got = decode[panelSettingsView](t, e.do("PUT", "/api/admin/panel", e.admin, map[string]any{"enabled": true, "refresh_min": 10, "rotation": 180, "controller": "ssd1680"}))
	if got.RefreshMin != 10 || got.Rotation != 180 || got.Controller != "ssd1680" {
		t.Fatalf("saved = %+v", got)
	}

	m := decode[panelMenuView](t, e.do("GET", "/api/admin/panel/menu", e.admin, nil))
	if m.Edited || len(m.Items) != len(menu.Defaults()) || len(m.Actions) == 0 {
		t.Fatalf("menu = %+v", m)
	}
	expect(t, e.do("PUT", "/api/admin/panel/menu", e.admin, map[string]any{"items": []map[string]any{
		{"label": "Reset", "action": "reset", "roles": "both", "enabled": true}}}), http.StatusBadRequest)
	m = decode[panelMenuView](t, e.do("PUT", "/api/admin/panel/menu", e.admin, map[string]any{"items": []map[string]any{
		{"label": "Close checkpoint", "action": "complete_race", "roles": "checkpoint", "enabled": true}}}))
	if !m.Edited || len(m.Items) != 1 || !m.Items[0].Confirm {
		t.Fatalf("edited = %+v", m)
	}
	m = decode[panelMenuView](t, e.do("POST", "/api/admin/panel/menu/reset", e.admin, nil))
	if m.Edited || len(m.Items) != len(menu.Defaults()) {
		t.Fatalf("after reset = %+v", m)
	}

	resp := e.do("GET", "/api/admin/panel/preview.png", e.admin, nil)
	expect(t, resp, http.StatusOK)
	img, err := png.Decode(resp.Body)
	if err != nil || img.Bounds().Dx() != 250 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("preview: %v %v %q", img, err, resp.Header.Get("Content-Type"))
	}
}

func TestPanelHookViewAndRequests(t *testing.T) {
	e := panelEnv(t, checkpointSettings(store.RaceActive))
	v := hookView(t, e)
	if v.Status.Role != "checkpoint" || v.Status.CPCode != "AS5" || v.Status.Port != 8090 || v.Status.StateLabel != "Checkpoint open" {
		t.Fatalf("status = %+v", v.Status)
	}
	avail := map[string]bool{}
	for _, it := range v.Menu {
		avail[it.Label] = it.Available
	}
	if !avail["Close checkpoint"] || avail["Open checkpoint"] || avail["Open race"] {
		t.Fatalf("availability = %v", avail)
	}
	if v.FullAllowedAt != nil {
		t.Fatal("floor set before any refresh")
	}
	at := time.Now().UTC().Truncate(time.Second)
	if code, _ := hookDo(t, e, "POST", "/api/hook/panel/refreshed", map[string]any{"at": at}); code != http.StatusOK {
		t.Fatalf("refreshed = %d", code)
	}
	seq := v.RefreshSeq
	expect(t, e.do("POST", "/api/admin/panel/refresh", e.admin, nil), http.StatusOK)
	expect(t, e.do("POST", "/api/admin/panel/test-pattern", e.admin, map[string]any{"controller": "ssd1675"}), http.StatusOK)
	expect(t, e.do("POST", "/api/admin/panel/test-pattern", e.admin, map[string]any{"controller": "bogus"}), http.StatusBadRequest)
	v = hookView(t, e)
	if v.FullAllowedAt == nil || !v.FullAllowedAt.Equal(at.Add(3*time.Minute)) || v.RefreshSeq == seq || v.TestPattern != "ssd1675" {
		t.Fatalf("view = %+v", v)
	}
	// Detect again: the controller is cleared for the wizard.
	_, _ = e.st.SavePanelSettings(ctx, store.PanelSettings{Enabled: true, RefreshMin: 5, Controller: "ssd1680z"})
	expect(t, e.do("POST", "/api/admin/panel/detect", e.admin, nil), http.StatusOK)
	if v = hookView(t, e); v.Settings.Controller != "" || !v.Settings.DetectPending {
		t.Fatalf("after detect = %+v", v.Settings)
	}
	// The wizard gives up: no more detection until asked again.
	if code, _ := hookDo(t, e, "POST", "/api/hook/panel/detect-gave-up", map[string]any{"gave_up": true}); code != http.StatusOK {
		t.Fatal("gave up not recorded")
	}
	if v = hookView(t, e); v.Settings.DetectPending {
		t.Fatal("detection still pending after giving up")
	}
	expect(t, e.do("POST", "/api/admin/panel/detect", e.admin, nil), http.StatusOK)
	if v = hookView(t, e); !v.Settings.DetectPending {
		t.Fatal("detect again after giving up")
	}
	if v.Boot == 0 {
		t.Fatal("no boot id")
	}
	if code, _ := hookDo(t, e, "PUT", "/api/hook/panel/controller", map[string]any{"controller": "ssd1680"}); code != http.StatusOK {
		t.Fatal("controller not saved")
	}
	if code, _ := hookDo(t, e, "PUT", "/api/hook/panel/controller", map[string]any{"controller": "x"}); code != http.StatusBadRequest {
		t.Fatal("bad controller accepted")
	}
	if v = hookView(t, e); v.Settings.Controller != "ssd1680" {
		t.Fatalf("controller = %q", v.Settings.Controller)
	}
}

func TestPanelHookActions(t *testing.T) {
	e := panelEnv(t, checkpointSettings(store.RaceActive))
	v := hookView(t, e)
	ids, acts := map[string]int64{}, map[string]menu.Action{}
	for _, it := range v.Menu {
		ids[it.Label], acts[it.Label] = it.ID, it.Action
	}
	if ids["Close checkpoint"] == 0 {
		t.Fatalf("menu has no IDs: %+v", v.Menu)
	}
	act := func(label string, body any) panel.ActionResult {
		t.Helper()
		code, raw := hookDo(t, e, "POST", "/api/hook/panel/actions/"+itoa64(ids[label]), map[string]any{"action": acts[label], "menu_rev": v.MenuRev})
		if code != http.StatusOK {
			t.Fatalf("%s = %d %s", label, code, raw)
		}
		var r panel.ActionResult
		_ = json.Unmarshal(raw, &r)
		return r
	}
	if r := act("Run link check", nil); !r.OK || !strings.Contains(r.Message, "N0CALL-10") {
		t.Fatalf("link check = %+v", r)
	}
	if r := act("Close checkpoint", nil); !r.OK {
		t.Fatalf("close = %+v", r)
	}
	if cfg, _ := e.st.GetSettings(ctx); cfg.RaceState != store.RaceComplete {
		t.Fatalf("state = %s", cfg.RaceState)
	}
	// Not available any more: refused with a reason, nothing changes.
	if r := act("Close checkpoint", nil); r.OK || r.Message == "" {
		t.Fatalf("second close = %+v", r)
	}
	// Local items never run in the app.
	if r := act("Status", nil); r.OK {
		t.Fatalf("status action ran in the app: %+v", r)
	}
	if code, _ := hookDo(t, e, "POST", "/api/hook/panel/actions/9999", map[string]any{"action": "status"}); code != http.StatusOK {
		t.Fatal("unknown item")
	}
	// A stale view: the ID now names a different action, or the menu
	// was edited since the panel's poll.
	code, raw := hookDo(t, e, "POST", "/api/hook/panel/actions/"+itoa64(ids["Secure for travel"]), map[string]any{"action": "check_in", "menu_rev": v.MenuRev})
	if code != http.StatusOK || !strings.Contains(string(raw), "changed") {
		t.Fatalf("stale action = %d %s", code, raw)
	}
	code, raw = hookDo(t, e, "POST", "/api/hook/panel/actions/"+itoa64(ids["Secure for travel"]), map[string]any{"action": "secure", "menu_rev": v.MenuRev + 1})
	if code != http.StatusOK || !strings.Contains(string(raw), "changed") {
		t.Fatalf("stale menu revision = %d %s", code, raw)
	}
}

func TestPanelHookHQTargets(t *testing.T) {
	e := panelEnv(t, hqSettings(store.RaceSetup))
	_ = e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS1", Name: "Creek", CourseOrder: 1, ExpectedCall: "N0CALL-1"})
	v := hookView(t, e)
	if len(v.Targets) != 1 || v.Targets[0].Call != "N0CALL-1" || v.Status.HQ == nil || v.Status.HQ.Listed != 1 {
		t.Fatalf("view = %+v", v)
	}
	var link int64
	for _, it := range v.Menu {
		if it.Action == menu.LinkCheck {
			link = it.ID
		}
	}
	code, raw := hookDo(t, e, "POST", "/api/hook/panel/actions/"+itoa64(link), map[string]any{"to": "N0CALL-1", "action": "link_check", "menu_rev": v.MenuRev})
	if code != http.StatusOK || !strings.Contains(string(raw), "N0CALL-1") {
		t.Fatalf("HQ link check = %d %s", code, raw)
	}
}

func panelAct(t *testing.T, e *env, a menu.Action) panel.ActionResult {
	t.Helper()
	v := hookView(t, e)
	for _, it := range v.Menu {
		if it.Action == a && it.Available {
			code, raw := hookDo(t, e, "POST", "/api/hook/panel/actions/"+itoa64(it.ID), map[string]any{"action": a, "menu_rev": v.MenuRev})
			if code != http.StatusOK {
				t.Fatalf("%s = %d", a, code)
			}
			var r panel.ActionResult
			_ = json.Unmarshal(raw, &r)
			return r
		}
	}
	t.Fatalf("%s not available", a)
	return panel.ActionResult{}
}

func TestPanelLifecycleFromThePanel(t *testing.T) {
	e := panelEnv(t, checkpointSettings(store.RaceSetup))
	for _, step := range []struct {
		a     menu.Action
		state string
		msg   string
	}{
		{menu.StartRace, store.RaceActive, "open"},
		{menu.CompleteRace, store.RaceComplete, "closed"},
		{menu.Secure, store.RaceSecured, "Secured"},
		{menu.CheckIn, store.RaceCheckingIn, "Checking in"},
	} {
		r := panelAct(t, e, step.a)
		cfg, _ := e.st.GetSettings(ctx)
		if !r.OK || !strings.Contains(r.Message, step.msg) || cfg.RaceState != step.state {
			t.Fatalf("%s: %+v, state %s", step.a, r, cfg.RaceState)
		}
	}
	hq := panelEnv(t, hqSettings(store.RaceSetup))
	if r := panelAct(t, hq, menu.StartRace); !strings.Contains(r.Message, "Race started") {
		t.Fatalf("HQ start = %+v", r)
	}
	if r := panelAct(t, hq, menu.CompleteRace); !strings.Contains(r.Message, "Race complete") {
		t.Fatalf("HQ complete = %+v", r)
	}
}

func TestPanelLinkCheckTooSoonAndViewDetails(t *testing.T) {
	e := panelEnv(t, checkpointSettings(store.RaceSetup))
	if r := panelAct(t, e, menu.LinkCheck); !r.OK {
		t.Fatalf("first = %+v", r)
	}
	// Finish it, then ask again right away.
	c, _ := e.st.GetLinkCheck(ctx, 1)
	now := time.Now()
	c.State, c.StartedAt, c.FinishedAt, c.Verdict, c.Uplink, c.Count = store.LinkCheckDone, &now, &now, "PASS", 5, 5
	_ = e.st.UpdateLinkCheck(ctx, c)
	if r := panelAct(t, e, menu.LinkCheck); r.OK || !strings.Contains(r.Message, "2 min apart") {
		t.Fatalf("second = %+v", r)
	}
	v := hookView(t, e)
	if v.Status.LastLink == nil || v.Status.LastLink.Verdict != "PASS" {
		t.Fatalf("last link = %+v", v.Status.LastLink)
	}
}

func TestPanelViewWarnings(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) {
		d.HookToken, d.Clock = hookTok, raceclockUnsynced()
	})
	e.inbox.status.AuthFailed = true
	v := hookView(t, e)
	if v.Status.GraywolfOK || !strings.Contains(v.Status.GraywolfProblem, "login") || len(v.Status.Warnings) != 1 {
		t.Fatalf("status = %+v", v.Status)
	}
}

func TestPanelHQCounts(t *testing.T) {
	e := panelEnv(t, hqSettings(store.RaceActive))
	_ = e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS1", Name: "Creek", CourseOrder: 1, ExpectedCall: "N0CALL-1"})
	hb := &store.Heartbeat{CP: "AS1", LastSeq: 0, Closed: true}
	if err := e.st.RecordHeartbeat(ctx, hb, "N0CALL-1", time.Now(), time.Now(), false); err != nil {
		t.Fatal(err)
	}
	v := hookView(t, e)
	if h := v.Status.HQ; h == nil || h.Listed != 1 || h.Heard != 1 || h.Closed != 1 {
		t.Fatalf("HQ = %+v", v.Status.HQ)
	}
}

func TestStateLabels(t *testing.T) {
	for _, c := range []struct{ state, role, want string }{
		{store.RaceSetup, store.RoleCheckpoint, "Checkpoint not open"},
		{store.RaceActive, store.RoleCheckpoint, "Checkpoint open"},
		{store.RaceComplete, store.RoleCheckpoint, "Checkpoint closed"},
		{store.RaceSecured, store.RoleCheckpoint, "Secured for travel"},
		{store.RaceCheckingIn, store.RoleCheckpoint, "Final check-in"},
		{store.RaceCheckedIn, store.RoleCheckpoint, "Checked in"},
		{store.RaceSetup, store.RoleHQ, "Race not started"},
		{store.RaceActive, store.RoleHQ, "Race active"},
		{store.RaceComplete, store.RoleHQ, "Race complete"},
		{"odd", store.RoleHQ, "odd"},
	} {
		if got := stateLabel(c.state, c.role); got != c.want {
			t.Errorf("%s/%s = %q", c.state, c.role, got)
		}
	}
}

func TestPanelBadBodies(t *testing.T) {
	e := panelEnv(t, checkpointSettings(store.RaceActive))
	resp := e.do("PUT", "/api/admin/panel", e.admin, "not an object")
	expect(t, resp, http.StatusBadRequest)
	expect(t, e.do("PUT", "/api/admin/panel/menu", e.admin, "x"), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/admin/panel/test-pattern", e.admin, "x"), http.StatusBadRequest)
	if code, _ := hookDo(t, e, "POST", "/api/hook/panel/actions/abc", map[string]any{"action": "status"}); code != http.StatusOK {
		t.Fatal("non-numeric id")
	}
	if code, _ := hookDo(t, e, "POST", "/api/hook/panel/refreshed", "x"); code != http.StatusBadRequest {
		t.Fatal("bad refreshed body")
	}
	// A panel clock far ahead never pushes the floor out.
	future := time.Now().Add(24 * time.Hour)
	hookDo(t, e, "POST", "/api/hook/panel/refreshed", map[string]any{"at": future})
	if v := hookView(t, e); v.FullAllowedAt == nil || v.FullAllowedAt.After(time.Now().Add(4*time.Minute)) {
		t.Fatalf("allowed at = %v", v.FullAllowedAt)
	}
}

func raceclockUnsynced() *raceclock.Clock { return raceclock.NewClock(nil, nil) }
