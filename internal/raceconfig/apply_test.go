package raceconfig

import (
	"context"
	"errors"
	"strings"
	"testing"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

var ctx = context.Background()

func rig(t *testing.T, cfg store.Settings) (*Service, *store.Store, *gwfake.Station) {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	gw := gwfake.New("N0CALL-1")
	return NewService(st, gw), st, gw
}

func freshNode() store.Settings { return store.DefaultSettings() }

// apply previews, then applies with that preview's token, as the UI does.
func apply(t *testing.T, svc *Service, f *File, station string, withGW bool) (Result, error) {
	t.Helper()
	p, err := svc.Preview(ctx, f, station)
	if err != nil {
		return Result{}, err
	}
	return svc.Apply(ctx, f, station, withGW, p.Token)
}

func changed(cs []Change, field string) *Change {
	for i := range cs {
		if cs[i].Field == field {
			return &cs[i]
		}
	}
	return nil
}

func TestPreviewCheckpoint(t *testing.T) {
	svc, _, _ := rig(t, freshNode())
	f, _ := parse(t, sample)
	p, err := svc.Preview(ctx, f, "AS1")
	if err != nil {
		t.Fatal(err)
	}
	if !p.CanApply || p.Role != store.RoleCheckpoint {
		t.Fatalf("plan = %+v", p)
	}
	for field, to := range map[string]string{"role": "checkpoint", "checkpoint_code": "AS1", "station_tactical": "Ridge Aid #1",
		"hq_call": "KD2DCM-3", "race_name": "Ridge 50K"} {
		if c := changed(p.Settings, field); c == nil || c.To != to {
			t.Errorf("%s change = %+v, want to %q", field, c, to)
		}
	}
	if c := changed(p.Settings, "heartbeat_sec"); c != nil {
		t.Errorf("heartbeat unchanged (300 = default) but listed: %+v", c)
	}
	if p.Checkpoints != nil {
		t.Error("a checkpoint node doesn't get HQ's checkpoint list")
	}
	if p.EventPage == nil {
		t.Error("event page change missing")
	}
	if c := changed(p.Graywolf, "callsign"); c == nil || c.From != "N0CALL-1" || c.To != "KD2DCM-4" {
		t.Errorf("graywolf callsign change = %+v", c)
	}
	if c := changed(p.Graywolf, "digipeater"); c != nil {
		t.Errorf("digipeater already off, but listed: %+v", c)
	}
}

func TestApplyHQWithGraywolfAndRestore(t *testing.T) {
	svc, st, gw := rig(t, freshNode())
	f, _ := parse(t, strings.Replace(sample, `"digipeater": false`, `"digipeater": true`, 1))
	res, err := apply(t, svc, f, HQ, true)
	if err != nil || len(res.GraywolfErrors) != 0 {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	set, _ := st.GetSettings(ctx)
	if set.Role != store.RoleHQ || set.HQLocalCodes != "START,FIN" || set.RaceName != "Ridge 50K" || set.StationTactical != "Race HQ" {
		t.Fatalf("settings = %+v", set)
	}
	cps, _ := st.ListCheckpoints(ctx)
	if len(cps) != 2 || cps[1].ExpectedCall != "KD2DCM-5" {
		t.Fatalf("checkpoints = %+v", cps)
	}
	if page, _, _ := st.EventPage(ctx); !strings.Contains(page, "145.050") {
		t.Fatalf("event page = %q", page)
	}
	sc, _ := gw.StationConfig(ctx)
	d, _ := gw.Digipeater(ctx)
	if sc.Callsign != "KD2DCM-3" || !d.Enabled {
		t.Fatalf("graywolf callsign %q digipeater %v", sc.Callsign, d.Enabled)
	}
	// Everything stays editable on the device: a later edit isn't undone.
	set.HeartbeatSec = 600
	if _, err := st.UpdateSettings(ctx, set); err != nil {
		t.Fatal(err)
	}
	// Restore puts graywolf back as it was before the first apply, even
	// after applying twice.
	if _, err := apply(t, svc, f, HQ, true); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RestoreGraywolf(ctx); err != nil {
		t.Fatal(err)
	}
	sc, _ = gw.StationConfig(ctx)
	d, _ = gw.Digipeater(ctx)
	if sc.Callsign != "N0CALL-1" || d.Enabled {
		t.Fatalf("after restore: callsign %q digipeater %v", sc.Callsign, d.Enabled)
	}
	if _, err := svc.RestoreGraywolf(ctx); !errors.Is(err, ErrNoBackup) {
		t.Fatalf("second restore err = %v", err)
	}
}

func TestApplyWithoutGraywolfLeavesItAlone(t *testing.T) {
	svc, _, gw := rig(t, freshNode())
	f, _ := parse(t, sample)
	if _, err := apply(t, svc, f, "AS2", false); err != nil {
		t.Fatal(err)
	}
	if sc, _ := gw.StationConfig(ctx); sc.Callsign != "N0CALL-1" {
		t.Fatalf("graywolf callsign changed without consent: %q", sc.Callsign)
	}
}

func TestApplyOnlyBeforeTheRace(t *testing.T) {
	cfg := freshNode()
	cfg.Role, cfg.CheckpointCode, cfg.HQCall, cfg.RaceState = store.RoleCheckpoint, "AS1", "KD2DCM-3", store.RaceActive
	svc, _, _ := rig(t, cfg)
	f, _ := parse(t, sample)
	p, err := svc.Preview(ctx, f, "AS1")
	if err != nil || p.CanApply || len(p.Problems) == 0 {
		t.Fatalf("preview mid-race = %+v, %v; want not applicable", p, err)
	}
	if _, err := apply(t, svc, f, "AS1", false); !errors.Is(err, ErrNotSetup) {
		t.Fatalf("apply mid-race err = %v", err)
	}
}

// A graywolf problem found at preview (no TX timing for the channel) is
// shown before anything is applied; the other changes still go through.
func TestGraywolfProblemsShownAtPreview(t *testing.T) {
	svc, _, gw := rig(t, freshNode())
	gw.EditRadio(func(r *gwfake.RadioSetup) { r.TxTimings = nil })
	f, _ := parse(t, sample)
	res, err := apply(t, svc, f, "AS1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Plan.Problems) != 1 || !strings.Contains(res.Plan.Problems[0], "TX timing") || len(res.GraywolfErrors) != 0 {
		t.Fatalf("problems %v, graywolf errors %v", res.Plan.Problems, res.GraywolfErrors)
	}
	if sc, _ := gw.StationConfig(ctx); sc.Callsign != "KD2DCM-4" {
		t.Fatalf("callsign not applied: %q", sc.Callsign)
	}
}

// A graywolf write that fails is reported; the rest still applies, and
// the backup lets the operator restore what did change.
func TestGraywolfWriteErrorsAreReported(t *testing.T) {
	svc, _, gw := rig(t, freshNode())
	f, _ := parse(t, strings.Replace(sample, `"tx_delay_ms": 300`, `"tx_delay_ms": 450`, 1))
	gw.FailNextSetTxTiming(errors.New("graywolf busy"))
	res, err := apply(t, svc, f, "AS1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GraywolfErrors) != 1 || !strings.Contains(res.GraywolfErrors[0], "TX timing") {
		t.Fatalf("graywolf errors = %v", res.GraywolfErrors)
	}
}

// Review H1/M5: the backup keeps the first original of every value a
// config changed, across applies; restore puts back only those fields.
func TestBackupMergesAndRestoresFieldsOnly(t *testing.T) {
	svc, _, gw := rig(t, freshNode())
	// First config changes only the callsign.
	a, _ := parse(t, `{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},
		"checkpoints":[{"code":"AS1","name":"Ridge","callsign":"KD2DCM-4"}]}`)
	if _, err := apply(t, svc, a, "AS1", true); err != nil {
		t.Fatal(err)
	}
	// A second one also changes the digipeater and TX delay.
	b, _ := parse(t, `{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},
		"checkpoints":[{"code":"AS1","name":"Ridge","callsign":"KD2DCM-7"}],"graywolf":{"digipeater":true,"tx_delay_ms":450}}`)
	if _, err := apply(t, svc, b, "AS1", true); err != nil {
		t.Fatal(err)
	}
	// The operator edits another TX timing field in graywolf meanwhile.
	tts, _ := gw.TxTimings(ctx)
	tts[0].Persist = 200
	_, _ = gw.SetTxTiming(ctx, tts[0])
	if _, err := svc.RestoreGraywolf(ctx); err != nil {
		t.Fatal(err)
	}
	sc, _ := gw.StationConfig(ctx)
	d, _ := gw.Digipeater(ctx)
	tts, _ = gw.TxTimings(ctx)
	if sc.Callsign != "N0CALL-1" || d.Enabled || tts[0].TxDelayMS != 300 {
		t.Fatalf("restored: callsign %q digipeater %v delay %d; want the values before the first config", sc.Callsign, d.Enabled, tts[0].TxDelayMS)
	}
	if tts[0].Persist != 200 {
		t.Fatalf("restore reverted the operator's own edit (persist %d)", tts[0].Persist)
	}
}

// Review M1: Apply does exactly what was previewed, or nothing.
func TestApplyRefusesAStalePreview(t *testing.T) {
	svc, st, _ := rig(t, freshNode())
	f, _ := parse(t, sample)
	p, err := svc.Preview(ctx, f, "AS1")
	if err != nil {
		t.Fatal(err)
	}
	// This node changes after the preview.
	cur, _ := st.GetSettings(ctx)
	cur.RaceName = "Changed meanwhile"
	_, _ = st.UpdateSettings(ctx, cur)
	if _, err := svc.Apply(ctx, f, "AS1", false, p.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("stale apply err = %v", err)
	}
	if _, err := svc.Apply(ctx, f, "AS1", false, "made-up"); !errors.Is(err, ErrStale) {
		t.Fatalf("bad token err = %v", err)
	}
	if got, _ := st.GetSettings(ctx); got.CheckpointCode != "" {
		t.Fatal("a stale apply changed settings")
	}
	// A different station is a different preview.
	p2, _ := svc.Preview(ctx, f, "AS2")
	if _, err := svc.Apply(ctx, f, "AS1", false, p2.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("other station's token err = %v", err)
	}
}

// Review L1, M2: stations must be in the file; TX timing problems show.
func TestPreviewChecks(t *testing.T) {
	svc, _, gw := rig(t, freshNode())
	noHQ, _ := parse(t, `{"format":"checkin-board-race/1","race":{"name":"x"}}`)
	if _, err := svc.Preview(ctx, noHQ, HQ); !errors.Is(err, ErrInvalid) {
		t.Fatalf("station not in the file err = %v", err)
	}
	gw.EditRadio(func(r *gwfake.RadioSetup) { r.TxTimings = nil })
	f, _ := parse(t, sample)
	p, err := svc.Preview(ctx, f, "AS1")
	if err != nil || len(p.Problems) != 1 || !strings.Contains(p.Problems[0], "TX timing") {
		t.Fatalf("problems = %v, %v", p.Problems, err)
	}
}

// Review M7: HQ with no local codes anywhere says what to do; the node's
// own codes mustn't clash with the file's checkpoints.
func TestHQLocalCodesAtPreview(t *testing.T) {
	f, _ := parse(t, `{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},
		"checkpoints":[{"code":"FIN","name":"Finish","callsign":"KD2DCM-4"}]}`)
	svc, _, _ := rig(t, freshNode())
	if _, err := svc.Preview(ctx, f, HQ); err == nil || !strings.Contains(err.Error(), "Station page") {
		t.Fatalf("no local codes err = %v", err)
	}
	node := freshNode()
	node.Role, node.HQLocalCodes = store.RoleHQ, "START,FIN"
	svc, _, _ = rig(t, node)
	if _, err := svc.Preview(ctx, f, HQ); err == nil || !strings.Contains(err.Error(), "FIN") {
		t.Fatalf("clashing local code err = %v", err)
	}
}
