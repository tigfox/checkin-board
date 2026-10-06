package ops

import (
	"bytes"
	"encoding/csv"
	"errors"
	"testing"
	"time"

	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

func seedHQ(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, hqSettings(), store.RaceActive)
	_ = e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Aid 5", CourseOrder: 1})
	_ = e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "FIN", Name: "=Finish", CourseOrder: 2})
	if _, err := e.st.UpsertRunners(ctx, []store.Runner{{Bib: 101, Category: "50K"}, {Bib: 102, Category: "+25K"}}); err != nil {
		t.Fatal(err)
	}
	msg, _ := wire.Decode("RC1 R AS5 1 @1300 101/00 999/10")
	if _, err := e.st.IngestReport(ctx, msg.(*wire.Report), "=N0CALL-1", 0, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	e.ft.Advance(time.Hour)
	e.logBib("FIN", 101)
	return e
}

func TestBoard(t *testing.T) {
	e := seedHQ(t)
	b, err := e.svc.Board(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Checkpoints) != 2 || b.Checkpoints[0].Code != "AS5" || !b.Checkpoints[1].Defined {
		t.Fatalf("columns = %+v", b.Checkpoints)
	}
	if len(b.Runners) != 3 {
		t.Fatalf("runners = %+v (roster 101,102 + unknown 999)", b.Runners)
	}
	r101, r102, r999 := b.Runners[0], b.Runners[1], b.Runners[2]
	if len(r101.Cells[0].Times) != 1 || len(r101.Cells[1].Times) != 1 || r101.LastSeenCP != "FIN" {
		t.Errorf("101 = %+v", r101)
	}
	if r102.LastSeenAt != nil || !r102.InRoster {
		t.Errorf("102 = %+v, want in roster but never seen", r102)
	}
	if r999.InRoster || r999.LastSeenCP != "AS5" {
		t.Errorf("999 = %+v, want an unknown bib flagged", r999)
	}
	if len(b.Health) != 2 {
		t.Errorf("health = %+v", b.Health)
	}
}

func TestRunnerHistory(t *testing.T) {
	e := seedHQ(t)
	h, err := e.svc.RunnerHistory(ctx, 101)
	if err != nil || !h.InRoster || h.Category != "50K" || len(h.Passages) != 2 {
		t.Fatalf("history = %+v, %v", h, err)
	}
	h, err = e.svc.RunnerHistory(ctx, 4321)
	if err != nil || h.InRoster || len(h.Passages) != 0 {
		t.Fatalf("unknown bib = %+v, %v", h, err)
	}
}

func TestExportResultsNeutralizesFormulas(t *testing.T) {
	e := seedHQ(t)
	var buf bytes.Buffer
	if err := e.svc.ExportResults(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil || len(rows) != 4 {
		t.Fatalf("rows = %v, %v", rows, err)
	}
	if rows[0][0] != "bib" || rows[0][7] != "count" {
		t.Fatalf("header = %v", rows[0])
	}
	for _, r := range rows[1:] {
		for _, cell := range []string{r[1], r[3], r[5]} {
			if cell != "" && (cell[0] == '=' || cell[0] == '+') {
				t.Errorf("formula not neutralized: %q in %v", cell, r)
			}
		}
	}
	last := rows[3]
	if last[3] != "'=Finish" || last[5] != "HQ" {
		t.Errorf("FIN row = %v", last)
	}
}

func TestBoardQueriesAreHQOnly(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	if _, err := e.svc.Board(ctx); !errors.Is(err, ErrWrongRole) {
		t.Errorf("Board err = %v", err)
	}
	if _, err := e.svc.RunnerHistory(ctx, 1); !errors.Is(err, ErrWrongRole) {
		t.Errorf("RunnerHistory err = %v", err)
	}
	if err := e.svc.ExportResults(ctx, &bytes.Buffer{}); !errors.Is(err, ErrWrongRole) {
		t.Errorf("ExportResults err = %v", err)
	}
}

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{"": "", "abc": "abc", "=1+1": "'=1+1", "+x": "'+x", "-x": "'-x", "@x": "'@x", "\tx": "'\tx"}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Errorf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
