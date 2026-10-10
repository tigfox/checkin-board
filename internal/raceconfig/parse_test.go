package raceconfig

import (
	"errors"
	"strings"
	"testing"

	"checkin-board/internal/store"
)

// sample is the proposal's example file (docs/specs/2026-10-10-race-config-proposal.md).
const sample = `{
  "format": "checkin-board-race/1",
  "race": { "name": "Ridge 50K" },
  "messaging": { "path": "", "heartbeat_sec": 300, "flush_after_sec": 20, "max_in_flight": 4,
                 "max_text_len": 67, "gap_grace_sec": 90 },
  "hq": { "callsign": "KD2DCM-3", "station_name": "Race HQ", "local_codes": ["START", "FIN"] },
  "checkpoints": [
    { "code": "AS1", "name": "Ridge", "station_name": "Ridge Aid #1", "callsign": "KD2DCM-4", "course_order": 1 },
    { "code": "AS2", "name": "Creek", "station_name": "Creek Aid", "callsign": "KD2DCM-5", "course_order": 2 }
  ],
  "event_page": "## Frequencies\n- Packet: 145.050 MHz\n- Voice net: 146.520 MHz",
  "graywolf": { "tx_delay_ms": 300, "tx_tail_ms": 100, "message_retention_days": 0, "digipeater": false }
}`

func parse(t *testing.T, s string) (*File, error) {
	t.Helper()
	return Parse(strings.NewReader(s))
}

func TestParseSample(t *testing.T) {
	f, err := parse(t, sample)
	if err != nil {
		t.Fatal(err)
	}
	if f.Race.Name != "Ridge 50K" || len(f.Checkpoints) != 2 || f.HQ.Callsign != "KD2DCM-3" ||
		f.Graywolf == nil || *f.Graywolf.TxDelayMS != 300 || *f.Graywolf.Digipeater {
		t.Fatalf("parsed = %+v", f)
	}
	st := f.Stations()
	if len(st) != 3 || st[0].ID != HQ || st[1].ID != "AS1" || st[1].Label != "AS1 Ridge (KD2DCM-4)" {
		t.Fatalf("stations = %+v", st)
	}
	if f.StationFor("kd2dcm-5") != "AS2" || f.StationFor("KD2DCM-3") != HQ || f.StationFor("W1AW") != "" {
		t.Fatalf("StationFor: %q %q %q", f.StationFor("kd2dcm-5"), f.StationFor("KD2DCM-3"), f.StationFor("W1AW"))
	}
}

func TestSettingsForStation(t *testing.T) {
	f, _ := parse(t, sample)
	base := store.DefaultSettings()
	base.GWChannel = 2 // not in the file: kept
	cp, err := f.SettingsFor("AS1", base)
	if err != nil {
		t.Fatal(err)
	}
	if cp.Role != store.RoleCheckpoint || cp.CheckpointCode != "AS1" || cp.StationTactical != "Ridge Aid #1" ||
		cp.HQCall != "KD2DCM-3" || cp.RaceName != "Ridge 50K" || cp.HeartbeatSec != 300 || cp.GWChannel != 2 {
		t.Fatalf("checkpoint settings = %+v", cp)
	}
	hq, err := f.SettingsFor(HQ, base)
	if err != nil || hq.Role != store.RoleHQ || hq.HQLocalCodes != "START,FIN" || hq.StationTactical != "Race HQ" {
		t.Fatalf("hq settings = %+v, %v", hq, err)
	}
	if _, err := f.SettingsFor("ZZ9", base); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown station err = %v", err)
	}
	if f.StationCallsign("AS2") != "KD2DCM-5" || f.StationCallsign(HQ) != "KD2DCM-3" {
		t.Fatal("StationCallsign")
	}
	list := f.CheckpointList()
	if len(list) != 2 || list[0].Code != "AS1" || list[0].Name != "Ridge" || list[0].ExpectedCall != "KD2DCM-4" || list[0].CourseOrder != 1 {
		t.Fatalf("checkpoint list = %+v", list)
	}
}

// Fields left out of the file leave the node's settings alone.
func TestPartialFileKeepsTheRest(t *testing.T) {
	f, err := parse(t, `{"format":"checkin-board-race/1","race":{"name":"Hill 10K"},
		"hq":{"callsign":"KD2DCM-3","local_codes":["FIN"]}}`)
	if err != nil {
		t.Fatal(err)
	}
	base := store.DefaultSettings()
	base.HeartbeatSec, base.StationTactical = 600, "Kept"
	got, err := f.SettingsFor(HQ, base)
	if err != nil || got.HeartbeatSec != 600 || got.StationTactical != "Kept" || got.RaceName != "Hill 10K" {
		t.Fatalf("settings = %+v, %v", got, err)
	}
}

func TestParseRejects(t *testing.T) {
	big := `{"format":"checkin-board-race/1","event_page":"` + strings.Repeat("x", MaxEventPageBytes+1) + `"}`
	cases := map[string]struct{ file, want string }{
		"not json":           {`{`, "not valid JSON"},
		"wrong format":       {`{"format":"something/2"}`, "format"},
		"unknown field":      {`{"format":"checkin-board-race/1","colour":"red"}`, "unknown field"},
		"a secret":           {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3","password":"x"}}`, "secret"},
		"trailing data":      {`{"format":"checkin-board-race/1"} {}`, "after the"},
		"bad code":           {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},"checkpoints":[{"code":"as-1","name":"x"}]}`, "checkpoints[0]"},
		"duplicate code":     {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},"checkpoints":[{"code":"AS1","name":"a"},{"code":"AS1","name":"b"}]}`, "twice"},
		"duplicate call":     {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3","local_codes":["FIN"]},"checkpoints":[{"code":"AS1","name":"a","callsign":"kd2dcm-3"}]}`, "twice"},
		"local code clash":   {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3","local_codes":["AS1"]},"checkpoints":[{"code":"AS1","name":"a"}]}`, "AS1"},
		"bad hq call":        {`{"format":"checkin-board-race/1","hq":{"callsign":"NETCONTROL"}}`, "hq"},
		"bad station name":   {`{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3","local_codes":["FIN"]},"checkpoints":[{"code":"AS1","name":"a","station_name":" padded"}]}`, "checkpoints[0]"},
		"checkpoints, no hq": {`{"format":"checkin-board-race/1","checkpoints":[{"code":"AS1","name":"a"}]}`, "hq.callsign is needed"},
		"bad heartbeat":      {`{"format":"checkin-board-race/1","messaging":{"heartbeat_sec":5}}`, "heartbeat"},
		"bad tx delay":       {`{"format":"checkin-board-race/1","graywolf":{"tx_delay_ms":99999}}`, "tx_delay_ms"},
		"event page size":    {big, "event_page"},
		"too big":            {`{"format":"checkin-board-race/1","race":{"name":"` + strings.Repeat("x", MaxFileBytes) + `"}}`, "larger than"},
	}
	for name, c := range cases {
		_, err := parse(t, c.file)
		if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrInvalid mentioning %q", name, err, c.want)
		}
	}
}

func FuzzParse(f *testing.F) {
	f.Add(sample)
	f.Add(`{"format":"checkin-board-race/1"}`)
	f.Fuzz(func(t *testing.T, s string) {
		file, err := Parse(strings.NewReader(s))
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error not ErrInvalid: %v", err)
			}
			return
		}
		for _, st := range file.Stations() {
			if _, err := file.SettingsFor(st.ID, file.validationBase()); err != nil {
				t.Fatalf("parsed file, but station %s doesn't apply: %v", st.ID, err)
			}
		}
	})
}

// HQ's local codes may come from the node rather than the file.
func TestHQWithoutLocalCodes(t *testing.T) {
	f, err := parse(t, `{"format":"checkin-board-race/1","hq":{"callsign":"KD2DCM-3"},
		"checkpoints":[{"code":"AS1","name":"Ridge","callsign":"KD2DCM-4"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	node := store.DefaultSettings()
	node.Role, node.HQLocalCodes = store.RoleHQ, "START"
	if got, err := f.SettingsFor(HQ, node); err != nil || got.HQLocalCodes != "START" {
		t.Fatalf("settings = %+v, %v", got, err)
	}
	if _, err := f.SettingsFor(HQ, store.DefaultSettings()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a node with no local codes either: err = %v, want the HQ rule", err)
	}
}
