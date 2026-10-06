package graywolf

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	f := newFakeGW(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	f.mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Health{Status: "ok", StartedAt: now.Add(-time.Hour), Time: now})
	})
	h, err := f.client(t).Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if h.Status != "ok" || !h.Time.Equal(now) {
		t.Errorf("got %+v", h)
	}
}

func TestStationConfigGetAndPut(t *testing.T) {
	f := newFakeGW(t)
	call := "N0CALL"
	f.mux.HandleFunc("GET /api/station/config", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, StationConfig{Callsign: call})
	})
	f.mux.HandleFunc("PUT /api/station/config", func(w http.ResponseWriter, r *http.Request) {
		var req StationConfig
		_ = json.NewDecoder(r.Body).Decode(&req)
		call = req.Callsign
		writeTestJSON(w, http.StatusOK, StationConfig{Callsign: call})
	})
	c := f.client(t)
	ctx := context.Background()

	got, err := c.StationConfig(ctx)
	if err != nil || got.Callsign != "N0CALL" {
		t.Fatalf("get: %+v %v", got, err)
	}
	got, err = c.SetStationCallsign(ctx, "n0call-9")
	if err != nil || got.Callsign != "N0CALL-9" {
		t.Fatalf("put: %+v %v", got, err)
	}
}

func TestSetStationCallsignValidates(t *testing.T) {
	c := newFakeGW(t).client(t)
	for _, bad := range []string{"", "TOOLONGCALL", "N0CALL-123", "N0 CALL"} {
		if _, err := c.SetStationCallsign(context.Background(), bad); err == nil {
			t.Errorf("SetStationCallsign(%q): expected error", bad)
		}
	}
}
