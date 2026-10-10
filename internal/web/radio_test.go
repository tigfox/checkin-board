package web

import (
	"net/http"
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
