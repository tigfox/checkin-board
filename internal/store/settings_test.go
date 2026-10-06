package store

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validCheckpoint() Settings {
	c := DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall = RoleCheckpoint, "AS5", "N0HQ-1"
	return c
}

func validHQ() Settings {
	c := DefaultSettings()
	c.Role, c.HQLocalCodes = RoleHQ, "START,FIN"
	return c
}

func TestGetSettingsDefaultsWithoutRow(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultSettings() {
		t.Errorf("got %+v, want defaults", got)
	}
	var n int64
	if err := s.db.Table("settings").Count(&n).Error; err != nil || n != 0 {
		t.Errorf("GetSettings created a row (%d, %v)", n, err)
	}
}

func TestSaveSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ft := &fakeTime{t: t0}
	s.SetClock(ft.Now)
	in := validCheckpoint()
	in.RaceName, in.StationTactical, in.Path, in.GWChannel = "Ridge 50K", "AID3", "WIDE1-1,WIDE2-1", 2
	started := t0.Add(-time.Hour).Add(300 * time.Millisecond)
	in.RaceState, in.RaceStartedAt = RaceActive, &started

	saved, err := s.SaveSettings(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.RaceStartedAt == nil || !got.RaceStartedAt.Equal(normTime(started)) {
		t.Fatalf("RaceStartedAt = %v, want %v", got.RaceStartedAt, normTime(started))
	}
	got.RaceStartedAt, saved.RaceStartedAt = nil, nil
	if got != saved || !got.UpdatedAt.Equal(t0) || got.ID != 1 {
		t.Fatalf("got %+v\nsaved %+v", got, saved)
	}
	// Saving again updates in place.
	in.RaceName = "Ridge 50K (rev)"
	if _, err := s.SaveSettings(ctx, in); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetSettings(ctx); got.RaceName != "Ridge 50K (rev)" {
		t.Errorf("RaceName = %q", got.RaceName)
	}
}

func TestSettingsValidate(t *testing.T) {
	mut := func(base Settings, f func(*Settings)) Settings {
		c := base
		f(&c)
		return c
	}
	valid := []Settings{
		DefaultSettings(),
		validCheckpoint(),
		validHQ(),
		mut(validCheckpoint(), func(c *Settings) { c.StationTactical = "FINISH-2" }),
		mut(validHQ(), func(c *Settings) { c.RaceState = RaceComplete }),
	}
	for i, c := range valid {
		if err := c.Validate(); err != nil {
			t.Errorf("valid[%d]: %v", i, err)
		}
	}
	invalid := map[string]Settings{
		"bad role":             mut(DefaultSettings(), func(c *Settings) { c.Role = "off" }),
		"bad state":            mut(validHQ(), func(c *Settings) { c.RaceState = "running" }),
		"active without role":  mut(DefaultSettings(), func(c *Settings) { c.RaceState = RaceActive }),
		"cp code":              mut(validCheckpoint(), func(c *Settings) { c.CheckpointCode = "as5" }),
		"cp missing hq":        mut(validCheckpoint(), func(c *Settings) { c.HQCall = "" }),
		"hq tactical not call": mut(validCheckpoint(), func(c *Settings) { c.HQCall = "NETCONTROL" }),
		"hq no codes":          mut(validHQ(), func(c *Settings) { c.HQLocalCodes = "" }),
		"hq dup codes":         mut(validHQ(), func(c *Settings) { c.HQLocalCodes = "FIN,FIN" }),
		"hq bad code":          mut(validHQ(), func(c *Settings) { c.HQLocalCodes = "FIN,fin" }),
		"race name long":       mut(validHQ(), func(c *Settings) { c.RaceName = strings.Repeat("x", MaxRaceNameLen+1) }),
		"race name control":    mut(validHQ(), func(c *Settings) { c.RaceName = "a\x00b" }),
		"tactical lower":       mut(validHQ(), func(c *Settings) { c.StationTactical = "aid3" }),
		"tactical dash first":  mut(validHQ(), func(c *Settings) { c.StationTactical = "-AID" }),
		"tactical long":        mut(validHQ(), func(c *Settings) { c.StationTactical = "ABCDEFGHIJ" }),
		"channel":              mut(validHQ(), func(c *Settings) { c.GWChannel = -1 }),
		"path":                 mut(validHQ(), func(c *Settings) { c.Path = "WIDE2-16" }),
		"text len":             mut(validHQ(), func(c *Settings) { c.MaxTextLen = 66 }),
		"flush":                mut(validHQ(), func(c *Settings) { c.FlushAfterSec = 1 }),
		"in flight":            mut(validHQ(), func(c *Settings) { c.MaxInFlight = 9 }),
		"heartbeat":            mut(validHQ(), func(c *Settings) { c.HeartbeatSec = 10 }),
		"gap grace":            mut(validHQ(), func(c *Settings) { c.GapGraceSec = 1000 }),
	}
	for name, c := range invalid {
		if err := c.Validate(); !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("%s: err = %v, want ErrInvalidSettings", name, err)
		}
	}
}

func TestSaveSettingsRejectsInvalid(t *testing.T) {
	s := newTestStore(t)
	bad := validHQ()
	bad.HQLocalCodes = ""
	if _, err := s.SaveSettings(ctx, bad); !errors.Is(err, ErrInvalidSettings) {
		t.Fatalf("err = %v", err)
	}
}

func TestLocalCodes(t *testing.T) {
	if got := validCheckpoint().LocalCodes(); len(got) != 1 || got[0] != "AS5" {
		t.Errorf("checkpoint = %v", got)
	}
	if got := validHQ().LocalCodes(); len(got) != 2 || got[1] != "FIN" {
		t.Errorf("hq = %v", got)
	}
	hq := validHQ()
	hq.HQLocalCodes = ""
	if got := hq.LocalCodes(); got != nil {
		t.Errorf("hq no codes = %v", got)
	}
	if got := DefaultSettings().LocalCodes(); got != nil {
		t.Errorf("unset = %v", got)
	}
}

func TestValidatePath(t *testing.T) {
	for _, ok := range []string{"", "WIDE2-1", "WIDE1-1,WIDE2-1", "RIDGE", "N0CALL-15", "A,B,C,D,E,F,G,H"} {
		if err := ValidatePath(ok); err != nil {
			t.Errorf("ValidatePath(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"wide2-1", "WIDE2-16", "WIDE2-", ",WIDE2-1", "WIDE1-1,", "WIDE1-1, WIDE2-1", "TOOLONGX", "A,B,C,D,E,F,G,H,I", "WIDE2*"} {
		if err := ValidatePath(bad); err == nil {
			t.Errorf("ValidatePath(%q) = nil, want error", bad)
		}
	}
}

func TestValidStationCall(t *testing.T) {
	for _, ok := range []string{"N0CALL", "K1ABC-9", "W1AW-15", "KK7ABC-7"} {
		if !ValidStationCall(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "n0call", "TOOLONG1", "K1ABC-123", "AID-3XY", "-K1", "N0CALL-16", "N0CALL-AB", "N0CALL-99"} {
		if ValidStationCall(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
