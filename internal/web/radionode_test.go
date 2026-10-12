package web

import (
	"context"
	"slices"
	"strings"
	"testing"

	"checkin-board/internal/radiobackend"
	"checkin-board/internal/radiocheck"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

// stubRadio is a radio node's radio for the web pages (phase 17d).
type stubRadio struct{ state radiobackend.State }

func (s *stubRadio) State() radiobackend.State { return s.state }
func (s *stubRadio) RadioStatus(context.Context) (uvk5.Status, error) {
	return uvk5.Status{Monitor: true, WouldTransmit: true, InBand: true, BatteryMV: 7400, Decoded: 3, FreqHz: 144_390_000, FreeSlots: 4}, nil
}

func connectedRadio() *stubRadio {
	return &stubRadio{state: radiobackend.State{Connected: true, Firmware: "f4hwn.aprs.2", Callsign: "KD2DCM-4", APRSOn: true,
		Caps: uvk5.Capabilities{Revision: 1, Features: 0x3ff, QueueDepth: 4, AckDepth: 4, MaxText: 67}}}
}

func TestRadioNodePages(t *testing.T) {
	radio := connectedRadio()
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) { d.HookToken, d.Radio = hookTok, radio })

	set := decode[settingsView](t, e.do("GET", "/api/admin/settings", e.admin, nil))
	if set.Backend != "uvk5" {
		t.Fatalf("backend = %q", set.Backend)
	}
	gv := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if gv.Backend != "uvk5" || !gv.Reachable || gv.Version != "f4hwn.aprs.2" || gv.Callsign != "KD2DCM-4" || len(gv.Warnings) != 0 {
		t.Fatalf("connection card = %+v", gv)
	}
	rv := decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if rv.Backend != "uvk5" || rv.Report.Status != radiocheck.OK || rv.Report.Items[0].Key != "radio" {
		t.Fatalf("radio check = %+v", rv)
	}
	if st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil)); st.Graywolf.Problem != "" {
		t.Fatalf("banner while connected = %q", st.Graywolf.Problem)
	}

	// Unplugged: the keypad, the panel and the connection card say so.
	radio.state = radiobackend.State{Error: "uvk5: no radio found"}
	if st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil)); st.Graywolf.Problem != radioDownWarning {
		t.Fatalf("keypad banner = %q", st.Graywolf.Problem)
	}
	if v := hookView(t, e); !slices.Contains(v.Status.Warnings, radioDownWarning) {
		t.Fatalf("panel warnings = %v", v.Status.Warnings)
	}
	gv = decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if gv.Reachable || len(gv.Warnings) != 1 || !strings.Contains(gv.Warnings[0], "no radio found") {
		t.Fatalf("connection card unplugged = %+v", gv)
	}
	rv = decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if rv.Report.Status != radiocheck.Fail {
		t.Fatalf("radio check unplugged = %+v", rv.Report)
	}
}

// A graywolf node says so too.
func TestGraywolfNodeBackend(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	if set := decode[settingsView](t, e.do("GET", "/api/admin/settings", e.admin, nil)); set.Backend != "graywolf" {
		t.Fatalf("backend = %q", set.Backend)
	}
	if gv := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil)); gv.Backend != "graywolf" {
		t.Fatalf("connection backend = %q", gv.Backend)
	}
}
