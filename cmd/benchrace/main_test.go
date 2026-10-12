package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/bench"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestUsageErrors(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want string
	}{
		"no nodes":    {nil, map[string]string{passwordEnv: "x"}, "-hq and at least one -cp"},
		"no password": {[]string{"-hq", "http://h", "-cp", "http://c"}, nil, passwordEnv},
		"bad flag":    {[]string{"-nope"}, nil, "flag provided but not defined"},
		"stray args":  {[]string{"-hq", "http://h", "-cp", "http://c", "http://d"}, map[string]string{passwordEnv: "x"}, "unexpected arguments"},
	} {
		var out, errOut strings.Builder
		if code := run(ctx, tc.args, env(tc.env), &out, &errOut); code != 2 || !strings.Contains(errOut.String(), tc.want) {
			t.Errorf("%s: code %d, stderr %q", name, code, errOut.String())
		}
	}
}

// Unreachable nodes are a setup error (exit 2), not a failed bench.
func TestUnreachableIsSetupError(t *testing.T) {
	var out, errOut strings.Builder
	code := run(context.Background(), []string{"-hq", "http://127.0.0.1:1", "-cp", "http://127.0.0.1:1"},
		env(map[string]string{passwordEnv: "x"}), &out, &errOut)
	if code != 2 || !strings.Contains(errOut.String(), "setup") {
		t.Fatalf("code %d, stderr %q", code, errOut.String())
	}
}

func TestPrintReport(t *testing.T) {
	var b strings.Builder
	printReport(&b, bench.Report{Expected: 2, Delivered: 1, Missing: []string{"AS1 bib 9002 at 13:00:01"},
		Latency: bench.Stats{N: 1, P50: time.Second, P95: time.Second, Max: time.Second},
		PerCP:   []bench.CPReport{{Code: "AS1", Expected: 2, Delivered: 1}}})
	for _, want := range []string{"FAIL: 1/2", "p50 1s", "AS1    1/2  no data", "missing: AS1 bib 9002"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, b.String())
		}
	}
}

func TestHelpExitsZero(t *testing.T) {
	var out, errOut strings.Builder
	if code := run(context.Background(), []string{"-h"}, env(nil), &out, &errOut); code != 0 {
		t.Fatalf("code %d", code)
	}
}
