package raceconfig

import (
	"bytes"
	"testing"

	"checkin-board/internal/store"
)

func TestExportRoundTrips(t *testing.T) {
	cfg := store.DefaultSettings()
	cfg.Role, cfg.HQLocalCodes, cfg.RaceName, cfg.StationTactical, cfg.HeartbeatSec = store.RoleHQ, "START,FIN", "Ridge 50K", "Race HQ", 600
	svc, st, gw := rig(t, cfg)
	_, _ = gw.SetStationCallsign(ctx, "KD2DCM-3")
	_ = st.ReplaceCheckpoints(ctx, []store.Checkpoint{{Code: "AS1", Name: "Ridge", CourseOrder: 1, ExpectedCall: "KD2DCM-4"}})
	_ = st.SetEventPage(ctx, "## Notes")
	data, err := svc.Export(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("export doesn't parse: %v\n%s", err, data)
	}
	if f.Race.Name != "Ridge 50K" || f.HQ.Callsign != "KD2DCM-3" || f.HQ.StationName != "Race HQ" || len(f.HQ.LocalCodes) != 2 ||
		len(f.Checkpoints) != 1 || f.Checkpoints[0].Callsign != "KD2DCM-4" || f.EventPage != "## Notes" ||
		f.Messaging == nil || *f.Messaging.HeartbeatSec != 600 || f.Messaging.GWChannel != nil || f.Graywolf != nil {
		t.Fatalf("exported = %+v", f)
	}
	// Loaded back on HQ, nothing changes.
	p, err := svc.Preview(ctx, f, HQ)
	if err != nil || len(p.Settings) != 0 || p.Checkpoints != nil || p.EventPage != nil || len(p.Graywolf) != 0 {
		t.Fatalf("re-applying the export = %+v, %v; want no changes", p, err)
	}
}

func TestExportNeedsHQ(t *testing.T) {
	svc, _, _ := rig(t, store.DefaultSettings())
	if _, err := svc.Export(ctx); err == nil {
		t.Fatal("export from a node that isn't HQ: want an error")
	}
}
