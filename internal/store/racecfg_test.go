package store

import (
	"errors"
	"strings"
	"testing"
)

func TestEventPage(t *testing.T) {
	s := newTestStore(t)
	if got, at, err := s.EventPage(ctx); err != nil || got != "" || at != nil {
		t.Fatalf("empty event page = %q %v %v", got, at, err)
	}
	if err := s.SetEventPage(ctx, "## Frequencies\n- 145.050"); err != nil {
		t.Fatal(err)
	}
	if got, at, _ := s.EventPage(ctx); got != "## Frequencies\n- 145.050" || at == nil {
		t.Fatalf("event page = %q %v", got, at)
	}
	if err := s.SetEventPage(ctx, strings.Repeat("x", MaxEventPageBytes+1)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized err = %v", err)
	}
	if err := s.SetEventPage(ctx, "a\xffb"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad utf-8 err = %v", err)
	}
	// Reset keeps it unless the race's reference data is cleared too.
	if err := s.ResetRaceData(ctx, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.EventPage(ctx); got == "" {
		t.Fatal("plain reset cleared the event page")
	}
	if err := s.ResetRaceData(ctx, ResetOptions{ClearReference: true}); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := s.EventPage(ctx); got != "" {
		t.Fatalf("event page after a reference reset = %q", got)
	}
}

func TestGraywolfBackup(t *testing.T) {
	s := newTestStore(t)
	if data, at, _ := s.GraywolfBackup(ctx); data != "" || at != nil {
		t.Fatalf("no backup = %q %v", data, at)
	}
	if err := s.PutGraywolfBackup(ctx, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	_ = s.PutGraywolfBackup(ctx, `{"a":1,"b":2}`)
	if data, at, _ := s.GraywolfBackup(ctx); data != `{"a":1,"b":2}` || at == nil {
		t.Fatalf("backup = %q %v", data, at)
	}
	if err := s.ClearGraywolfBackup(ctx); err != nil {
		t.Fatal(err)
	}
	if data, _, _ := s.GraywolfBackup(ctx); data != "" {
		t.Fatalf("after clear = %q", data)
	}
}

// Applying a race config is all or nothing, and only before the race.
func TestApplyRaceConfigAtomic(t *testing.T) {
	s := newTestStore(t)
	c := DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceName = RoleHQ, "FIN", "Ridge 50K"
	page := "## Notes"
	list := []Checkpoint{{Code: "AS1", Name: "Ridge"}}
	if err := s.ApplyRaceConfig(ctx, c, list, &page); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetSettings(ctx)
	cps, _ := s.ListCheckpoints(ctx)
	text, _, _ := s.EventPage(ctx)
	if got.RaceName != "Ridge 50K" || len(cps) != 1 || text != page {
		t.Fatalf("applied: %+v %+v %q", got, cps, text)
	}
	// A bad checkpoint row: the settings change is rolled back too.
	c.RaceName = "Other"
	if err := s.ApplyRaceConfig(ctx, c, []Checkpoint{{Code: "bad code", Name: "x"}}, nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad row err = %v", err)
	}
	if got, _ := s.GetSettings(ctx); got.RaceName != "Ridge 50K" {
		t.Fatalf("settings changed by a failed apply: %q", got.RaceName)
	}
	// Once the race has started: refused, in the same transaction.
	if _, err := s.SetRaceState(ctx, []string{RaceSetup}, RaceActive, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyRaceConfig(ctx, c, nil, nil); !errors.Is(err, ErrNotSetup) {
		t.Fatalf("mid-race err = %v", err)
	}
}

func TestReplaceCheckpoints(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateCheckpoint(ctx, &Checkpoint{Code: "OLD", Name: "Old"})
	list := []Checkpoint{{Code: "AS1", Name: "Ridge", CourseOrder: 1, ExpectedCall: "KD2DCM-4"}, {Code: "AS2", Name: "Creek", CourseOrder: 2}}
	if err := s.ReplaceCheckpoints(ctx, list); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListCheckpoints(ctx)
	if len(got) != 2 || got[0].Code != "AS1" || got[0].ExpectedCall != "KD2DCM-4" || got[1].Code != "AS2" {
		t.Fatalf("checkpoints = %+v", got)
	}
	// A bad row changes nothing.
	if err := s.ReplaceCheckpoints(ctx, []Checkpoint{{Code: "AS3", Name: "x"}, {Code: "bad code", Name: "y"}}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad row err = %v", err)
	}
	if got, _ := s.ListCheckpoints(ctx); len(got) != 2 {
		t.Fatalf("a failed replace changed the list: %+v", got)
	}
}
