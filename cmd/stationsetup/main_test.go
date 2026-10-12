package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"checkin-board/internal/store"
	"checkin-board/internal/testnode"
)

const pw = "setup-admin-password"

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

type result struct {
	code        int
	out, errOut string
}

func runWith(t *testing.T, stdin string, args ...string) result {
	t.Helper()
	var out, errOut strings.Builder
	code := run(context.Background(), args, env(map[string]string{passwordEnv: pw}), strings.NewReader(stdin), &out, &errOut)
	return result{code, out.String(), errOut.String()}
}

func writeCSV(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stations.csv")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func twoNodes(t *testing.T) (testnode.Node, testnode.Node, string) {
	hq := testnode.Start(t, testnode.Options{Call: "N0CALL", Password: pw})
	cp := testnode.Start(t, testnode.Options{Call: "N0CALL", Password: pw})
	csv := writeCSV(t, fmt.Sprintf("address,role,station_name,code,callsign,course_order,race_name\n"+
		"%s,hq,Finish Line,FIN,KD2DCM-1,,Michaux Mud\n%s,checkpoint,Cattle Chute,CTL,KD2DCM-4,1,\n", hq.URL, cp.URL))
	return hq, cp, csv
}

func TestUsage(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), []string{"x.csv"}, env(nil), strings.NewReader(""), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), passwordEnv) {
		t.Fatalf("no password: %d %q", code, errOut.String())
	}
	if r := runWith(t, ""); r.code != 2 || !strings.Contains(r.errOut, "usage") {
		t.Fatalf("no file: %+v", r)
	}
	if r := runWith(t, "", "-h"); r.code != 0 {
		t.Fatalf("-h: %+v", r)
	}
	if r := runWith(t, "", "/no/such.csv"); r.code != 2 {
		t.Fatalf("missing file: %+v", r)
	}
	bad := writeCSV(t, "address,role\n")
	if r := runWith(t, "", bad); r.code != 2 || !strings.Contains(r.errOut, "missing column") {
		t.Fatalf("bad CSV: %+v", r)
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	_, cp, csv := twoNodes(t)
	r := runWith(t, "", "-dry-run", csv)
	t.Logf("output:\n%s", r.out)
	if r.code != 0 || !strings.Contains(r.out, "Cattle Chute") || !strings.Contains(r.out, "-> KD2DCM-4") || !strings.Contains(r.out, "Dry run: 2 of 2") {
		t.Fatalf("dry run: %+v", r)
	}
	if s, _ := cp.Store.GetSettings(context.Background()); s.Role != "" {
		t.Fatalf("dry run changed the node: %+v", s)
	}
}

func TestDeclineChangesNothing(t *testing.T) {
	_, cp, csv := twoNodes(t)
	r := runWith(t, "no\n", csv)
	if r.code != 1 || !strings.Contains(r.out, "Type yes") || !strings.Contains(r.out, "Nothing changed") {
		t.Fatalf("declined: %+v", r)
	}
	if s, _ := cp.Store.GetSettings(context.Background()); s.Role != "" {
		t.Fatalf("declining changed the node: %+v", s)
	}
}

func TestSetUp(t *testing.T) {
	hq, cp, csv := twoNodes(t)
	r := runWith(t, "yes\n", csv)
	if r.code != 0 || strings.Count(r.out, ": set up\n") != 2 {
		t.Fatalf("setup: %+v", r)
	}
	if strings.Contains(r.out+r.errOut, pw) {
		t.Fatal("the password is in the output")
	}
	ctx := context.Background()
	if s, _ := cp.Store.GetSettings(ctx); s.Role != store.RoleCheckpoint || s.HQCall != "KD2DCM-1" {
		t.Fatalf("checkpoint = %+v", s)
	}
	if sc, _ := hq.GW.StationConfig(ctx); sc.Callsign != "KD2DCM-1" {
		t.Fatalf("HQ graywolf callsign = %q", sc.Callsign)
	}
	// Again, unattended: nothing left to change.
	if r := runWith(t, "", "-yes", csv); r.code != 0 || strings.Count(r.out, "nothing to change") != 2 {
		t.Fatalf("second run: %+v", r)
	}
}

func TestNoGraywolfLeavesCallsign(t *testing.T) {
	hq, _, csv := twoNodes(t)
	if r := runWith(t, "", "-yes", "-no-graywolf", csv); r.code != 0 || !strings.Contains(r.out, "graywolf (skipped)") {
		t.Fatalf("no-graywolf: %+v", r)
	}
	if sc, _ := hq.GW.StationConfig(context.Background()); sc.Callsign != "N0CALL" {
		t.Fatalf("callsign changed: %q", sc.Callsign)
	}
}

// One unreachable node: the others are set up; exit 1.
func TestUnreachableNode(t *testing.T) {
	hq := testnode.Start(t, testnode.Options{Call: "N0CALL", Password: pw})
	csv := writeCSV(t, fmt.Sprintf("address,role,station_name,code,callsign,course_order\n%s,hq,HQ,FIN,KD2DCM-1,\nhttp://127.0.0.1:1,checkpoint,Aid,AS1,KD2DCM-4,1\n", hq.URL))
	r := runWith(t, "", "-yes", csv)
	if r.code != 1 || !strings.Contains(r.out, "ERROR: login") || !strings.Contains(r.out, "[1] HQ: set up") || !strings.Contains(r.out, "NOT set up") {
		t.Fatalf("unreachable: %+v", r)
	}
}
