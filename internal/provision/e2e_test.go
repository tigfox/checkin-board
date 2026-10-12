package provision

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"checkin-board/internal/store"
	"checkin-board/internal/testnode"
)

const adminPW = "setup-admin-password"

func startNode(t *testing.T, call string) testnode.Node {
	t.Helper()
	return testnode.Start(t, testnode.Options{Call: call, Password: adminPW})
}

func TestSetupThreeNodes(t *testing.T) {
	ctx := context.Background()
	hq, cp1, cp2 := startNode(t, "N0CALL"), startNode(t, "N0CALL"), startNode(t, "N0CALL")
	csv := fmt.Sprintf(`address,role,station_name,code,callsign,course_order,race_name,path,heartbeat_sec,tx_delay_ms
%s,hq,Finish Line,START;FIN,KD2DCM-1,,Michaux Mud,WIDE1-1,600,250
%s,checkpoint,Cattle Chute,CTL,KD2DCM-4,1,,,,
%s,checkpoint,Ridge Top,RDG,KD2DCM-3,2,,,,
`, hq.URL, cp1.URL, cp2.URL)
	p, err := Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Password: adminPW, Graywolf: true}
	nodes := PreviewAll(ctx, p, opt)
	for _, n := range nodes {
		if !n.Ready() || len(n.Preview.Settings) == 0 || len(n.Preview.Graywolf) == 0 {
			t.Fatalf("%s: not ready: err %v, preview %+v", n.Address, n.Err, n.Preview)
		}
	}
	ApplyAll(ctx, p, nodes, opt)
	for _, n := range nodes {
		if !n.Applied || n.Err != nil || len(n.GraywolfErrors) > 0 {
			t.Fatalf("%s: applied %v, err %v, graywolf %v", n.Address, n.Applied, n.Err, n.GraywolfErrors)
		}
	}

	hs, _ := hq.Store.GetSettings(ctx)
	if hs.Role != store.RoleHQ || hs.RaceName != "Michaux Mud" || hs.HQLocalCodes != "START,FIN" || hs.Path != "WIDE1-1" || hs.HeartbeatSec != 600 {
		t.Fatalf("HQ settings = %+v", hs)
	}
	cps, _ := hq.Store.ListCheckpoints(ctx)
	if len(cps) != 2 || cps[0].Code != "CTL" || cps[0].ExpectedCall != "KD2DCM-4" || cps[1].CourseOrder != 2 {
		t.Fatalf("HQ checkpoints = %+v", cps)
	}
	c1, _ := cp1.Store.GetSettings(ctx)
	if c1.Role != store.RoleCheckpoint || c1.CheckpointCode != "CTL" || c1.HQCall != "KD2DCM-1" || c1.StationTactical != "Cattle Chute" || c1.HeartbeatSec != 600 {
		t.Fatalf("checkpoint settings = %+v", c1)
	}
	for call, n := range map[string]testnode.Node{"KD2DCM-1": hq, "KD2DCM-4": cp1, "KD2DCM-3": cp2} {
		sc, _ := n.GW.StationConfig(ctx)
		if sc.Callsign != call {
			t.Errorf("graywolf callsign = %q, want %q", sc.Callsign, call)
		}
	}

	// Running it again is harmless: nothing left to change.
	nodes = PreviewAll(ctx, p, opt)
	for _, n := range nodes {
		if !n.Ready() || len(n.Preview.Settings) != 0 || len(n.Preview.Graywolf) != 0 {
			t.Fatalf("%s: second run would change %+v / %+v (err %v)", n.Address, n.Preview.Settings, n.Preview.Graywolf, n.Err)
		}
	}
}

// A wrong password or an unreachable node is reported per node; the
// others still preview.
func TestSetupReportsBadNodes(t *testing.T) {
	ctx := context.Background()
	hq := startNode(t, "N0CALL")
	csv := fmt.Sprintf("address,role,station_name,code,callsign,course_order\n%s,hq,HQ,FIN,KD2DCM-1,\nhttp://127.0.0.1:1,checkpoint,Aid,AS1,KD2DCM-4,1\n", hq.URL)
	p, err := Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	nodes := PreviewAll(ctx, p, Options{Password: adminPW})
	if !nodes[0].Ready() || nodes[1].Ready() || nodes[1].Err == nil {
		t.Fatalf("nodes = %+v / %+v", nodes[0], nodes[1])
	}
	nodes = PreviewAll(ctx, p, Options{Password: "wrong"})
	if nodes[0].Err == nil || !strings.Contains(nodes[0].Err.Error(), "login") {
		t.Fatalf("wrong password: %v", nodes[0].Err)
	}
	ApplyAll(ctx, p, nodes, Options{Password: "wrong"}) // nothing ready: a no-op
	if nodes[0].Applied {
		t.Fatal("applied without a preview")
	}
}

// A radio node (UV-K5, phase 17) is set up from the same CSV: its race
// settings, and the callsign on the radio.
func TestSetupRadioNode(t *testing.T) {
	ctx := context.Background()
	hq := startNode(t, "N0CALL")
	rnode := testnode.Start(t, testnode.Options{Call: "W1RAD-1", Password: adminPW, UVK5: true})
	csv := fmt.Sprintf("address,role,station_name,code,callsign,course_order,race_name\n"+
		"%s,hq,Finish,FIN,KD2DCM-1,,Michaux Mud\n%s,checkpoint,Cattle Chute,CTL,KD2DCM-4,1,\n", hq.URL, rnode.URL)
	// The backend column must match the node.
	wrong, err := Parse(strings.NewReader(strings.NewReplacer("race_name\n", "race_name,backend\n", "Michaux Mud\n", "Michaux Mud,\n", "CTL,KD2DCM-4,1,\n", "CTL,KD2DCM-4,1,,graywolf\n").Replace(csv)))
	if err != nil {
		t.Fatal(err)
	}
	if n := PreviewAll(ctx, wrong, Options{Password: adminPW}); n[1].Err == nil || !strings.Contains(n[1].Err.Error(), "uses uvk5") {
		t.Fatalf("backend mismatch: %v", n[1].Err)
	}
	p, err := Parse(strings.NewReader(csv))
	if err != nil {
		t.Fatal(err)
	}
	opt := Options{Password: adminPW, Graywolf: true}
	nodes := PreviewAll(ctx, p, opt)
	if !nodes[1].Ready() || len(nodes[1].Preview.Graywolf) != 1 || !strings.Contains(nodes[1].Preview.Graywolf[0].Label, "radio") {
		t.Fatalf("radio node preview: err %v, %+v", nodes[1].Err, nodes[1].Preview)
	}
	ApplyAll(ctx, p, nodes, opt)
	if !nodes[1].Applied || len(nodes[1].GraywolfErrors) > 0 {
		t.Fatalf("radio node: applied %v, err %v, %v", nodes[1].Applied, nodes[1].Err, nodes[1].GraywolfErrors)
	}
	if got := rnode.UV.Callsign(); got != "KD2DCM-4" {
		t.Fatalf("radio callsign = %q", got)
	}
	if s, _ := rnode.Store.GetSettings(ctx); s.CheckpointCode != "CTL" || s.HQCall != "KD2DCM-1" {
		t.Fatalf("radio node settings = %+v", s)
	}
}
