package bench

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 11, 13, 0, 0, 0, time.UTC)

func TestAnalyze(t *testing.T) {
	logged := []Logged{
		{"AS1", 9001, t0},
		{"AS1", 9002, t0.Add(time.Second)},
		{"AS2", 9001, t0.Add(time.Minute)},
		{"AS2", 9002, t0.Add(2 * time.Minute)},
	}
	results := []Result{
		{"AS1", 9001, t0, t0.Add(10 * time.Second), 1},
		{"AS1", 9002, t0.Add(time.Second), t0.Add(31 * time.Second), 1},
		{"AS2", 9001, t0.Add(time.Minute), t0.Add(time.Minute + 20*time.Second), 2}, // counted twice
		{"AS2", 7, t0, t0, 1},                     // outside the bench's bibs: ignored
		{"AS1", 9003, t0, t0.Add(time.Second), 1}, // never logged
	}
	r := Analyze(logged, nil, results, 9001, 9100)
	if r.Expected != 4 || r.Delivered != 3 || r.OK() {
		t.Fatalf("report = %+v", r)
	}
	if len(r.Missing) != 1 || r.Missing[0] != "AS2 bib 9002 at 13:02:00" {
		t.Fatalf("missing = %v", r.Missing)
	}
	if len(r.Duplicates) != 1 || len(r.Unexpected) != 1 {
		t.Fatalf("dups = %v, unexpected = %v", r.Duplicates, r.Unexpected)
	}
	if r.Latency.N != 3 || r.Latency.Max != 30*time.Second || r.Latency.P50 != 20*time.Second {
		t.Fatalf("latency = %+v", r.Latency)
	}
	if len(r.PerCP) != 2 || r.PerCP[0].Code != "AS1" || r.PerCP[0].Delivered != 2 || r.PerCP[1].Delivered != 1 {
		t.Fatalf("per checkpoint = %+v", r.PerCP)
	}
}

func TestAnalyzeAllDelivered(t *testing.T) {
	logged := []Logged{{"AS1", 9001, t0}}
	r := Analyze(logged, nil, []Result{{"AS1", 9001, t0, t0.Add(5 * time.Second), 1}}, 9001, 9001)
	if !r.OK() {
		t.Fatalf("report = %+v", r)
	}
	if Analyze(nil, nil, nil, 1, 1).OK() {
		t.Fatal("an empty bench must not pass")
	}
}

// Two result rows for the same passage are a duplicate too.
func TestAnalyzeDuplicateRows(t *testing.T) {
	logged := []Logged{{"AS1", 9001, t0}}
	row := Result{"AS1", 9001, t0, t0.Add(5 * time.Second), 1}
	r := Analyze(logged, nil, []Result{row, row}, 9001, 9001)
	if len(r.Duplicates) != 1 || r.Latency.N != 1 {
		t.Fatalf("report = %+v", r)
	}
}

// A bib the bench gave up on (no reply) may still have been logged.
func TestAnalyzeGaveUpIsNotUnexpected(t *testing.T) {
	logged := []Logged{{"AS1", 9001, t0}}
	results := []Result{
		{"AS1", 9001, t0, t0.Add(5 * time.Second), 1},
		{"AS1", 9002, t0, t0.Add(-2 * time.Second), 1}, // gave up on; also clock skew
	}
	r := Analyze(logged, []Attempt{{"AS1", 9002}}, results, 9001, 9002)
	if !r.OK() || len(r.Unconfirmed) != 1 || len(r.Unexpected) != 0 {
		t.Fatalf("report = %+v", r)
	}
	r = Analyze([]Logged{{"AS1", 9002, t0}}, nil, results[1:], 9001, 9002)
	if r.NegativeLatency != 1 {
		t.Fatalf("negative latency = %d", r.NegativeLatency)
	}
}
