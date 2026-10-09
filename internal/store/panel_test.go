package store

import (
	"errors"
	"testing"
	"time"

	"checkin-board/internal/panel/menu"
)

func TestPanelSettingsDefaultsAndValidation(t *testing.T) {
	s := newTestStore(t)
	got, err := s.GetPanelSettings(ctx)
	if err != nil || !got.Enabled || got.Controller != "" || got.RefreshMin != 5 || got.Rotation != 0 || got.LastFullAt != nil {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	ok := PanelSettings{Enabled: true, Controller: ControllerSSD1680Z, RefreshMin: 10, Rotation: 180}
	if _, err := s.SavePanelSettings(ctx, ok); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]PanelSettings{
		"refresh under 3 min": {RefreshMin: 2},
		"refresh over 30":     {RefreshMin: 31},
		"rotation":            {RefreshMin: 5, Rotation: 90},
		"controller":          {RefreshMin: 5, Controller: "il0373"},
	} {
		if _, err := s.SavePanelSettings(ctx, bad); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if err := s.MarkPanelFullRefresh(ctx, t0); err != nil {
		t.Fatal(err)
	}
	// Saving settings keeps the refresh time; the controller can be set alone.
	if err := s.SetPanelController(ctx, ControllerSSD1675); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetPanelSettings(ctx)
	if got.RefreshMin != 10 || got.Controller != ControllerSSD1675 || got.LastFullAt == nil || !got.LastFullAt.Equal(t0) {
		t.Fatalf("settings = %+v", got)
	}
	if err := s.SetPanelController(ctx, "bogus"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bogus controller: %v", err)
	}
}

func TestPanelMenuDefaultsUntilEdited(t *testing.T) {
	s := newTestStore(t)
	items, edited, err := s.PanelMenu(ctx)
	if err != nil || edited || len(items) != len(menu.Defaults()) {
		t.Fatalf("fresh menu = %d items, edited %v, %v", len(items), edited, err)
	}
	mine, _ := menu.Validate([]menu.Item{
		{Label: "Close checkpoint", Action: menu.CompleteRace, Roles: menu.RolesCheckpoint, Enabled: true},
		{Label: "Status", Action: menu.Status, Roles: menu.RolesBoth, Enabled: false},
	})
	if err := s.ReplacePanelMenu(ctx, mine); err != nil {
		t.Fatal(err)
	}
	items, edited, _ = s.PanelMenu(ctx)
	if !edited || len(items) != 2 || items[0].Label != "Close checkpoint" || !items[0].Confirm || items[1].Enabled || items[0].ID == 0 {
		t.Fatalf("edited menu = %+v", items)
	}
	// Reset keeps the panel's configuration (it's node setup, like branding).
	if err := s.ResetRaceData(ctx, ResetOptions{ClearReference: true}); err != nil {
		t.Fatal(err)
	}
	if items, _, _ = s.PanelMenu(ctx); len(items) != 2 {
		t.Fatalf("menu after reset = %+v", items)
	}
	// An empty menu is a valid edit (the panel shows status only).
	if err := s.ReplacePanelMenu(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if items, edited, _ = s.PanelMenu(ctx); !edited || len(items) != 0 {
		t.Fatalf("emptied menu = %+v, edited %v", items, edited)
	}
	_ = time.Second
}

func TestPanelFloorNeverMovesBack(t *testing.T) {
	s := newTestStore(t)
	_ = s.MarkPanelFullRefresh(ctx, t0.Add(time.Hour))
	_ = s.MarkPanelFullRefresh(ctx, t0) // an older report
	if p, _ := s.GetPanelSettings(ctx); !p.LastFullAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last full = %v, moved back", p.LastFullAt)
	}
}

func TestPanelDetectionGaveUpAndMenuRevision(t *testing.T) {
	s := newTestStore(t)
	if err := s.PanelDetectionGaveUp(ctx); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPanelSettings(ctx); !p.DetectGaveUp || p.Controller != "" {
		t.Fatalf("settings = %+v", p)
	}
	// Detect again (controller "") clears it; so does finding one.
	_ = s.SetPanelController(ctx, "")
	if p, _ := s.GetPanelSettings(ctx); p.DetectGaveUp {
		t.Fatal("detect again kept gave-up")
	}
	_ = s.PanelDetectionGaveUp(ctx)
	_ = s.SetPanelController(ctx, ControllerSSD1680)
	if p, _ := s.GetPanelSettings(ctx); p.DetectGaveUp {
		t.Fatal("found controller kept gave-up")
	}
	p0, _ := s.GetPanelSettings(ctx)
	items, _ := menu.Validate(menu.Defaults()[:2])
	_ = s.ReplacePanelMenu(ctx, items)
	p1, _ := s.GetPanelSettings(ctx)
	_ = s.ResetPanelMenu(ctx)
	p2, _ := s.GetPanelSettings(ctx)
	if !(p0.MenuRev < p1.MenuRev && p1.MenuRev < p2.MenuRev) {
		t.Fatalf("menu revisions %d %d %d", p0.MenuRev, p1.MenuRev, p2.MenuRev)
	}
}
