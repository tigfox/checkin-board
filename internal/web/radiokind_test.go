package web

import (
	"net/http"
	"testing"

	"checkin-board/internal/store"
)

// The Station page's radio type: listed, chosen, and the app restarted
// with it, also mid-race.
func TestRadioKindSwitch(t *testing.T) {
	restarts := 0
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) {
		d.RadioKind, d.GraywolfReady, d.Restart = "graywolf", true, func() { restarts++ }
	})
	v := decode[radioKindView](t, e.do("GET", "/api/admin/radio-kind", e.admin, nil))
	if v.Current != "graywolf" || v.Chosen != "graywolf" || len(v.Kinds) < 2 || !v.Kinds[0].Available || !v.Kinds[1].Available {
		t.Fatalf("view = %+v", v)
	}
	// The same kind: saved, no restart.
	out := decode[map[string]any](t, e.do("PUT", "/api/admin/radio-kind", e.admin, radioKindBody{Kind: "graywolf"}))
	if out["restarting"] != false || restarts != 0 {
		t.Fatalf("same kind: %v, %d restarts", out, restarts)
	}
	out = decode[map[string]any](t, e.do("PUT", "/api/admin/radio-kind", e.admin, radioKindBody{Kind: "UVK5"}))
	if out["kind"] != "uvk5" || out["restarting"] != true || restarts != 1 {
		t.Fatalf("switch: %v, %d restarts", out, restarts)
	}
	if k, _ := e.st.RadioKind(ctx); k != "uvk5" {
		t.Fatalf("saved kind = %q", k)
	}
	v = decode[radioKindView](t, e.do("GET", "/api/admin/radio-kind", e.admin, nil))
	if v.Current != "graywolf" || v.Chosen != "uvk5" {
		t.Fatalf("before the restart = %+v", v)
	}
	expect(t, e.do("PUT", "/api/admin/radio-kind", e.admin, radioKindBody{Kind: "aprsd"}), http.StatusBadRequest)
	expect(t, e.do("PUT", "/api/admin/radio-kind", e.volunt, radioKindBody{Kind: "uvk5"}), http.StatusForbidden)
}

// graywolf can't be chosen on a node without graywolf's login.
func TestRadioKindNeedsGraywolf(t *testing.T) {
	e := newEnvWith(t, checkpointSettings(store.RaceSetup), func(d *Deps) {
		d.RadioKind, d.Radio = "uvk5", connectedRadio()
	})
	v := decode[radioKindView](t, e.do("GET", "/api/admin/radio-kind", e.admin, nil))
	if v.Current != "uvk5" || v.Kinds[0].ID != "graywolf" || v.Kinds[0].Available || v.Kinds[0].Why == "" {
		t.Fatalf("view = %+v", v)
	}
	expect(t, e.do("PUT", "/api/admin/radio-kind", e.admin, radioKindBody{Kind: "graywolf"}), http.StatusConflict)
	if k, _ := e.st.RadioKind(ctx); k != "" {
		t.Fatalf("saved kind = %q", k)
	}
}
