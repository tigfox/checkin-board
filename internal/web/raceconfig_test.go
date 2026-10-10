package web

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"checkin-board/internal/raceconfig"
	"checkin-board/internal/store"
)

const raceFile = `{
  "format": "checkin-board-race/1",
  "race": { "name": "Ridge 50K" },
  "hq": { "callsign": "KD2DCM-3", "station_name": "Race HQ", "local_codes": ["START", "FIN"] },
  "checkpoints": [
    { "code": "AS1", "name": "Ridge", "station_name": "Ridge Aid #1", "callsign": "N0CALL-1", "course_order": 1 },
    { "code": "AS2", "name": "Creek", "callsign": "KD2DCM-5", "course_order": 2 }
  ],
  "event_page": "## Frequencies\n- Packet: 145.050 MHz",
  "graywolf": { "digipeater": true }
}`

func TestRaceConfigFlow(t *testing.T) {
	e := newEnvWith(t, store.DefaultSettings(), func(d *Deps) { d.RaceConfigDir = t.TempDir() })
	// Upload: the name is made safe; invalid content is refused.
	expect(t, e.upload("POST", "/api/admin/raceconfigs", e.admin, "bad.json", []byte(`{"format":"x"}`)), http.StatusBadRequest)
	up := decode[raceconfig.Entry](t, e.upload("POST", "/api/admin/raceconfigs", e.admin, "Ridge 50K.json", []byte(raceFile)))
	if up.Name != "Ridge-50K.json" || up.RaceName != "Ridge 50K" {
		t.Fatalf("uploaded = %+v", up)
	}
	// The same name again isn't silently replaced.
	expect(t, e.upload("POST", "/api/admin/raceconfigs", e.admin, "Ridge 50K.json", []byte(raceFile)), http.StatusConflict)
	list := decode[raceConfigList](t, e.do("GET", "/api/admin/raceconfigs", e.admin, nil))
	if len(list.Files) != 1 || !list.CanApply || list.GraywolfBackup {
		t.Fatalf("list = %+v", list)
	}
	// Stations, with this node pre-selected by graywolf's callsign (N0CALL-1).
	st := decode[raceConfigStations](t, e.do("GET", "/api/admin/raceconfigs/Ridge-50K.json", e.admin, nil))
	if len(st.Stations) != 3 || st.Suggested != "AS1" {
		t.Fatalf("stations = %+v", st)
	}
	p := decode[raceconfig.Plan](t, e.do("POST", "/api/admin/raceconfigs/Ridge-50K.json/preview", e.admin, map[string]string{"station": "AS1"}))
	if !p.CanApply || len(p.Settings) == 0 || len(p.Graywolf) != 1 || p.Graywolf[0].Field != "digipeater" {
		t.Fatalf("preview = %+v", p)
	}
	// Apply needs the preview's token; a made-up one is refused.
	expect(t, e.do("POST", "/api/admin/raceconfigs/Ridge-50K.json/apply", e.admin,
		map[string]any{"station": "AS1", "graywolf": true, "token": "made-up"}), http.StatusConflict)
	res := decode[raceconfig.Result](t, e.do("POST", "/api/admin/raceconfigs/Ridge-50K.json/apply", e.admin,
		map[string]any{"station": "AS1", "graywolf": true, "token": p.Token}))
	if len(res.GraywolfErrors) != 0 {
		t.Fatalf("apply = %+v", res)
	}
	cfg, _ := e.st.GetSettings(ctx)
	if cfg.Role != store.RoleCheckpoint || cfg.CheckpointCode != "AS1" || cfg.HQCall != "KD2DCM-3" {
		t.Fatalf("settings after apply = %+v", cfg)
	}
	if d, _ := e.gw.Digipeater(ctx); !d.Enabled {
		t.Fatal("graywolf digipeater not applied")
	}
	// The event page is readable by volunteers, editable by the admin.
	page := decode[eventPageView](t, e.do("GET", "/api/eventpage", e.volunt, nil))
	if !strings.Contains(page.Text, "145.050") {
		t.Fatalf("event page = %+v", page)
	}
	expect(t, e.do("PUT", "/api/admin/eventpage", e.admin, map[string]string{"text": "edited on the node"}), http.StatusOK)
	if page := decode[eventPageView](t, e.do("GET", "/api/eventpage", e.volunt, nil)); page.Text != "edited on the node" {
		t.Fatalf("edited event page = %+v", page)
	}
	// Restore graywolf, then nothing left to restore.
	if l := decode[raceConfigList](t, e.do("GET", "/api/admin/raceconfigs", e.admin, nil)); !l.GraywolfBackup {
		t.Fatal("no graywolf backup after applying graywolf changes")
	}
	expect(t, e.do("POST", "/api/admin/graywolf/restore", e.admin, nil), http.StatusOK)
	if d, _ := e.gw.Digipeater(ctx); d.Enabled {
		t.Fatal("restore didn't turn the digipeater back off")
	}
	expect(t, e.do("POST", "/api/admin/graywolf/restore", e.admin, nil), http.StatusConflict)
	// Unknown station / file.
	expect(t, e.do("POST", "/api/admin/raceconfigs/Ridge-50K.json/preview", e.admin, map[string]string{"station": "ZZ9"}), http.StatusBadRequest)
	expect(t, e.do("GET", "/api/admin/raceconfigs/missing.json", e.admin, nil), http.StatusNotFound)
	expect(t, e.do("GET", "/api/admin/raceconfigs/..%2fsecret.json", e.admin, nil), http.StatusBadRequest)
	expect(t, e.do("DELETE", "/api/admin/raceconfigs/Ridge-50K.json", e.admin, nil), http.StatusOK)
	if l := decode[raceConfigList](t, e.do("GET", "/api/admin/raceconfigs", e.admin, nil)); len(l.Files) != 0 {
		t.Fatalf("after delete = %+v", l)
	}
}

func TestRaceConfigOnlyBeforeTheRace(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) { d.RaceConfigDir = t.TempDir() })
	expect(t, e.upload("POST", "/api/admin/raceconfigs", e.admin, "r.json", []byte(raceFile)), http.StatusOK)
	p := decode[raceconfig.Plan](t, e.do("POST", "/api/admin/raceconfigs/r.json/preview", e.admin, map[string]string{"station": "AS1"}))
	if p.CanApply {
		t.Fatal("preview mid-race says it can apply")
	}
	expect(t, e.do("POST", "/api/admin/raceconfigs/r.json/apply", e.admin, map[string]any{"station": "AS1", "token": p.Token}), http.StatusConflict)
}

func TestRaceConfigExport(t *testing.T) {
	e := newEnvWith(t, hqSettings(store.RaceSetup), func(d *Deps) { d.RaceConfigDir = t.TempDir() })
	_ = e.st.ReplaceCheckpoints(ctx, []store.Checkpoint{{Code: "AS1", Name: "Ridge", CourseOrder: 1, ExpectedCall: "KD2DCM-4"}})
	resp := e.do("GET", "/api/admin/raceconfig/export", e.admin, nil)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") ||
		!strings.Contains(string(body), `"checkin-board-race/1"`) || !strings.Contains(string(body), "KD2DCM-4") {
		t.Fatalf("export = %d %q %s", resp.StatusCode, resp.Header.Get("Content-Disposition"), body)
	}
	// Not HQ: no export.
	c := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) { d.RaceConfigDir = t.TempDir() })
	expect(t, c.do("GET", "/api/admin/raceconfig/export", c.admin, nil), http.StatusConflict)
}

// A full-size event page (64 KB of text) saves from the editor.
func TestEventPageFullSize(t *testing.T) {
	e := newEnv(t, store.DefaultSettings())
	page := strings.Repeat("\"quoted\" ", 8000)[:store.MaxEventPageBytes]
	expect(t, e.do("PUT", "/api/admin/eventpage", e.admin, map[string]string{"text": page}), http.StatusOK)
	expect(t, e.do("PUT", "/api/admin/eventpage", e.admin, map[string]string{"text": page + "x"}), http.StatusBadRequest)
}
