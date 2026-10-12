package provision

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"checkin-board/internal/raceconfig"
)

func TestExampleStationsCSV(t *testing.T) {
	f, err := os.Open("../../example_stations.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	p, err := Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.RaceName != "Michaux Mud" || len(p.Targets) != 3 {
		t.Fatalf("plan = %+v", p)
	}
	if p.Targets[0].Station != raceconfig.HQ || p.Targets[1].Station != "CTL" || p.Targets[2].Station != "RDG" {
		t.Fatalf("targets = %+v", p.Targets)
	}
	rc, err := raceconfig.Parse(bytes.NewReader(p.Config))
	if err != nil {
		t.Fatal(err)
	}
	if rc.HQ.Callsign != "KD2DCM-1" || len(rc.HQ.LocalCodes) != 2 || len(rc.Checkpoints) != 2 {
		t.Fatalf("config = %+v", rc)
	}
	if rc.Checkpoints[0].Name != "Cattle Chute Aid" || rc.Checkpoints[1].Name != "Ridge Top" || rc.Checkpoints[1].CourseOrder != 2 {
		t.Fatalf("checkpoints = %+v", rc.Checkpoints)
	}
	if rc.Messaging == nil || *rc.Messaging.Path != "WIDE1-1" || *rc.Messaging.HeartbeatSec != 300 {
		t.Fatalf("messaging = %+v", rc.Messaging)
	}
	if rc.Graywolf == nil || *rc.Graywolf.TxDelayMS != 300 || rc.Graywolf.Digipeater != nil {
		t.Fatalf("graywolf = %+v", rc.Graywolf)
	}
}

const header = "address,role,station_name,code,callsign,course_order\n"

func TestMinimalCSV(t *testing.T) {
	p, err := Parse(strings.NewReader(header + "10.0.1.88,HQ,Finish,,KD2DCM-1,\n10.0.1.183,checkpoint,Aid 1,as1,KD2DCM-4,1\n"))
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := raceconfig.Parse(bytes.NewReader(p.Config))
	if rc.Messaging != nil || rc.Graywolf != nil || rc.Race.Name != "" || p.Targets[1].Station != "AS1" {
		t.Fatalf("config = %+v, targets %+v", rc, p.Targets)
	}
}

func TestDirectPath(t *testing.T) {
	p, err := Parse(strings.NewReader("address,role,station_name,code,callsign,path\n10.0.1.88,hq,HQ,,KD2DCM-1,direct\n"))
	if err != nil {
		t.Fatal(err)
	}
	rc, _ := raceconfig.Parse(bytes.NewReader(p.Config))
	if rc.Messaging == nil || rc.Messaging.Path == nil || *rc.Messaging.Path != "" {
		t.Fatalf("messaging = %+v", rc.Messaging)
	}
}

func TestCSVErrors(t *testing.T) {
	hq := "10.0.1.88,hq,Finish,,KD2DCM-1,\n"
	for name, tc := range map[string]struct{ csv, want string }{
		"empty":                  {"", "header"},
		"no stations":            {header, "no stations"},
		"unknown column":         {"address,role,station_name,code,callsign,password\n", `unknown column "password"`},
		"missing column":         {"address,role,station_name,code\n", `missing column "callsign"`},
		"no HQ":                  {header + "10.0.1.183,checkpoint,Aid,AS1,KD2DCM-4,1\n", "no HQ row"},
		"two HQs":                {header + hq + "10.0.1.89,hq,HQ2,,KD2DCM-2,\n", "second HQ"},
		"bad role":               {header + hq + "10.0.1.183,relay,Aid,AS1,KD2DCM-4,1\n", "role"},
		"empty callsign":         {header + hq + "10.0.1.183,checkpoint,Aid,AS1,,1\n", "line 3: callsign is empty"},
		"checkpoint code":        {header + hq + "10.0.1.183,checkpoint,Aid,,KD2DCM-4,1\n", "line 3: code is empty"},
		"same address":           {header + hq + "10.0.1.88,checkpoint,Aid,AS1,KD2DCM-4,1\n", "same node as line 2"},
		"bad order":              {header + hq + "10.0.1.183,checkpoint,Aid,AS1,KD2DCM-4,first\n", "course_order"},
		"same callsign":          {header + hq + "10.0.1.183,checkpoint,Aid,AS1,KD2DCM-1,1\n", "race config"},
		"same code":              {header + hq + "10.0.1.183,checkpoint,A,AS1,KD2DCM-4,1\n10.0.1.184,checkpoint,B,AS1,KD2DCM-5,2\n", "race config"},
		"race-wide differs":      {"address,role,station_name,code,callsign,heartbeat_sec\n10.0.1.88,hq,HQ,,KD2DCM-1,300\n10.0.1.183,checkpoint,A,AS1,KD2DCM-4,600\n", "race-wide"},
		"not a number":           {"address,role,station_name,code,callsign,tx_delay_ms\n10.0.1.88,hq,HQ,,KD2DCM-1,soon\n", "whole number"},
		"out of range":           {"address,role,station_name,code,callsign,tx_delay_ms\n10.0.1.88,hq,HQ,,KD2DCM-1,99999\n", "race config"},
		"same node, other form":  {header + hq + "http://10.0.1.88:8090,checkpoint,Aid,AS1,KD2DCM-4,1\n", "same node as line 2"},
		"credentials in address": {header + "http://u:pw@10.0.1.88,hq,HQ,FIN,KD2DCM-1,\n", "no user or password"},
		"duplicate column":       {"address,role,station_name,code,callsign,callsign\n", "appears twice"},
		"too big":                {header + strings.Repeat("#"+strings.Repeat("x", 1000)+"\n", 300), "larger than"},
		"name too long":          {header + "10.0.1.88,hq,A name much longer than twenty five,,KD2DCM-1,\n", "race config"},
	} {
		_, err := Parse(strings.NewReader(tc.csv))
		if !errors.Is(err, ErrCSV) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
}

// A spreadsheet's byte-order mark and blank lines are fine.
func TestSpreadsheetQuirks(t *testing.T) {
	if _, err := Parse(strings.NewReader("\ufeff" + header + "10.0.1.88,hq,Finish,,KD2DCM-1,\n,,,,,\n")); err != nil {
		t.Fatal(err)
	}
}

// Race-wide values that mean the same don't conflict.
func TestRaceWideNormalized(t *testing.T) {
	csv := "address,role,station_name,code,callsign,path,heartbeat_sec\n" +
		"10.0.1.88,hq,HQ,FIN,KD2DCM-1,\"WIDE1-1, WIDE2-1\",300\n" +
		"10.0.1.183,checkpoint,Aid,AS1,KD2DCM-4,\"wide1-1,wide2-1\",0300\n"
	if _, err := Parse(strings.NewReader(csv)); err != nil {
		t.Fatal(err)
	}
}

// A byte-order mark before a quoted header.
func TestBOMQuotedHeader(t *testing.T) {
	csv := "\ufeff\"address\",\"role\",\"station_name\",\"code\",\"callsign\"\n10.0.1.88,hq,HQ,FIN,KD2DCM-1\n"
	if _, err := Parse(strings.NewReader(csv)); err != nil {
		t.Fatal(err)
	}
}

func TestFileNameFollowsContent(t *testing.T) {
	a, _ := Parse(strings.NewReader(header + "10.0.1.88,hq,HQ,FIN,KD2DCM-1,\n"))
	b, _ := Parse(strings.NewReader(header + "10.0.1.88,hq,HQ,FIN,KD2DCM-1,\n"))
	c, _ := Parse(strings.NewReader(header + "10.0.1.88,hq,HQ,FIN,KD2DCM-2,\n"))
	if a.FileName() != b.FileName() || a.FileName() == c.FileName() || !strings.HasPrefix(a.FileName(), "stationsetup-") {
		t.Fatalf("names %q %q %q", a.FileName(), b.FileName(), c.FileName())
	}
}
