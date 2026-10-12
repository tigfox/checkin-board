package web

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"checkin-board/internal/hostmon"
	"checkin-board/internal/radiocheck"
	"checkin-board/internal/store"
)

type fakeHost struct{ snap hostmon.Snapshot }

func (f *fakeHost) Snapshot() hostmon.Snapshot { return f.snap }

func TestRadioCheck(t *testing.T) {
	up := true
	host := &fakeHost{snap: hostmon.Snapshot{Machine: "aarch64", ModemKeepingUp: &up, ModemWaitsPerSec: 40}}
	e := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) { d.Host = host })
	got := decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if got.Report.Status != radiocheck.OK || got.Host.Machine != "aarch64" || len(got.Report.Items) == 0 {
		t.Fatalf("radio = %+v, want ok on a faster Pi with the default fake setup", got)
	}
	// The same setup on a Pi Zero: stock build and 48 kHz both fail.
	host.snap.Machine = "armv6l"
	got = decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if got.Report.Status != radiocheck.Fail {
		t.Fatalf("radio on a Pi Zero = %+v, want fail", got.Report)
	}
	failed := map[string]bool{}
	for _, it := range got.Report.Items {
		if it.Status == radiocheck.Fail {
			failed[it.Key] = true
		}
	}
	if !failed["build"] || !failed["rate"] {
		t.Fatalf("failed items = %v, want build and rate", failed)
	}
	expect(t, e.do("GET", "/api/admin/radio", e.volunt, nil), http.StatusForbidden)
}

// Without a host monitor (tests, non-Linux) the check still runs.
func TestRadioCheckWithoutHostMonitor(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	got := decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if len(got.Report.Items) == 0 || got.Host.Machine != "" {
		t.Fatalf("radio = %+v", got)
	}
}

// Field test 2026-10-11: the AIOC dropped off USB; graywolf's API still
// listed it. The node's own card list shows it gone.
func TestRadioCheckCableMissing(t *testing.T) {
	up := true
	host := &fakeHost{snap: hostmon.Snapshot{Machine: "aarch64", ModemKeepingUp: &up,
		SoundCardsKnown: true, SoundCards: []hostmon.SoundCard{{Index: 1, ID: "vc4hdmi"}}}}
	e := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) { d.Host = host })
	got := decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if a := radioItem(t, got.Report, "audio"); a.Status != radiocheck.Fail {
		t.Fatalf("audio = %+v, want fail", a)
	}

	// graywolf on another machine: this node's cards say nothing about it.
	e = newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) { d.Host, d.GraywolfRemote = host, true })
	got = decode[radioView](t, e.do("GET", "/api/admin/radio", e.admin, nil))
	if a := radioItem(t, got.Report, "audio"); a.Status != radiocheck.OK {
		t.Fatalf("remote graywolf: audio = %+v, want ok", a)
	}
	if got.Host.SoundCardsKnown || got.Host.SoundCards != nil {
		t.Fatalf("remote graywolf: host cards should be hidden, got %+v", got.Host)
	}
}

type fakeCable struct{ missing []string }

func (f *fakeCable) Missing() []string { return f.missing }

func TestCableMissingWarnings(t *testing.T) {
	cable := &fakeCable{missing: []string{"AllInOneCable"}}
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) { d.HookToken, d.Cable = hookTok, cable })
	if v := hookView(t, e); !slices.Contains(v.Status.Warnings, cableWarning) {
		t.Fatalf("panel warnings = %v", v.Status.Warnings)
	}
	gv := decode[graywolfView](t, e.do("GET", "/api/admin/gw", e.admin, nil))
	if !slices.ContainsFunc(gv.Warnings, func(w string) bool { return strings.Contains(w, "AllInOneCable") }) {
		t.Fatalf("admin warnings = %v", gv.Warnings)
	}
	st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil))
	if st.Graywolf.Problem != cableWarning {
		t.Fatalf("keypad banner = %q", st.Graywolf.Problem)
	}

	cable.missing = nil
	if v := hookView(t, e); slices.Contains(v.Status.Warnings, cableWarning) {
		t.Fatalf("after replug: warnings = %v", v.Status.Warnings)
	}
	if st := decode[stationView](t, e.do("GET", "/api/station", e.volunt, nil)); st.Graywolf.Problem != "" {
		t.Fatalf("after replug: banner = %q", st.Graywolf.Problem)
	}
}

func radioItem(t *testing.T, r radiocheck.Report, key string) radiocheck.Item {
	t.Helper()
	for _, it := range r.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("no %q item in %+v", key, r.Items)
	return radiocheck.Item{}
}
