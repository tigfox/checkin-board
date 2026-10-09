package store

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"checkin-board/internal/panel/epd"
	"checkin-board/internal/panel/menu"
)

// Node panel (spec 8.4): the e-ink display's settings and button menu.
// Node configuration, so Reset keeps it.

// Controllers re-exports the e-ink controllers (epd.Controllers); ""
// means not yet known: the panel detects it.
const (
	ControllerSSD1680Z = epd.SSD1680Z
	ControllerSSD1680  = epd.SSD1680
	ControllerSSD1675  = epd.SSD1675
)

// Panel refresh limits: Adafruit warns against refreshing an e-ink
// panel more often than every 3 minutes long-term.
const (
	PanelMinRefreshMin = 3
	PanelMaxRefreshMin = 30
)

// PanelSettings is the panel's configuration (singleton row).
type PanelSettings struct {
	ID         int        `gorm:"column:id;primaryKey"`
	Enabled    bool       `gorm:"column:enabled"`
	Controller string     `gorm:"column:controller"`
	RefreshMin int        `gorm:"column:refresh_min"`
	Rotation   int        `gorm:"column:rotation"`
	LastFullAt *time.Time `gorm:"column:last_full_at"`
	MenuEdited bool       `gorm:"column:menu_edited"`
	MenuRev    int64      `gorm:"column:menu_rev"`
	// DetectGaveUp: no controller was confirmed; detect again on request.
	DetectGaveUp bool      `gorm:"column:detect_gave_up"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (PanelSettings) TableName() string { return "panel_settings" }

// defaultPanelSettings: the controller defaults to the current bonnet
// revision. (The button wizard that detected it is shelved with the
// buttons; older bonnets are chosen on Admin > Panel.)
func defaultPanelSettings() PanelSettings {
	return PanelSettings{ID: 1, Enabled: true, RefreshMin: 5, Controller: epd.SSD1680Z}
}

func validController(c string) bool { return c == "" || epd.Known(c) }

// GetPanelSettings returns the panel settings (defaults if never saved).
func (s *Store) GetPanelSettings(ctx context.Context) (PanelSettings, error) {
	var p PanelSettings
	err := s.db.WithContext(ctx).Where("id = 1").Take(&p).Error
	if err == gorm.ErrRecordNotFound {
		return defaultPanelSettings(), nil
	}
	if err != nil {
		return PanelSettings{}, mapDBError(err)
	}
	p.LastFullAt = normTimePtr(p.LastFullAt)
	return p, nil
}

// SavePanelSettings validates and saves the admin-editable settings
// (enabled, controller, refresh interval, rotation). The last refresh
// time and the menu-edited flag are left alone.
func (s *Store) SavePanelSettings(ctx context.Context, p PanelSettings) (PanelSettings, error) {
	switch {
	case p.RefreshMin < PanelMinRefreshMin || p.RefreshMin > PanelMaxRefreshMin:
		return PanelSettings{}, fmt.Errorf("%w: the refresh interval must be %d-%d minutes", ErrInvalidInput, PanelMinRefreshMin, PanelMaxRefreshMin)
	case p.Rotation != 0 && p.Rotation != 180:
		return PanelSettings{}, fmt.Errorf("%w: rotation must be 0 or 180", ErrInvalidInput)
	case !validController(p.Controller):
		return PanelSettings{}, fmt.Errorf("%w: unknown controller %q", ErrInvalidInput, p.Controller)
	}
	err := s.upsertPanel(ctx, map[string]any{"enabled": p.Enabled, "controller": p.Controller,
		"refresh_min": p.RefreshMin, "rotation": p.Rotation})
	if err != nil {
		return PanelSettings{}, err
	}
	return s.GetPanelSettings(ctx)
}

// SetPanelController records the detected (or chosen) controller; ""
// asks the panel to detect it again.
func (s *Store) SetPanelController(ctx context.Context, c string) error {
	if !validController(c) {
		return fmt.Errorf("%w: unknown controller %q", ErrInvalidInput, c)
	}
	return s.upsertPanel(ctx, map[string]any{"controller": c, "detect_gave_up": false})
}

// PanelDetectionGaveUp records that the wizard found no readable
// controller; it runs again only when an admin asks (SetPanelController "").
func (s *Store) PanelDetectionGaveUp(ctx context.Context) error {
	return s.upsertPanel(ctx, map[string]any{"controller": "", "detect_gave_up": true})
}

// MarkPanelFullRefresh records a full refresh of the display. The time
// only moves forward, so a late or skewed report can't shorten the floor.
func (s *Store) MarkPanelFullRefresh(ctx context.Context, at time.Time) error {
	t := normTime(at)
	return s.upsertPanel(ctx, map[string]any{"last_full_at": gorm.Expr(
		"CASE WHEN last_full_at IS NULL OR last_full_at < ? THEN ? ELSE last_full_at END", t, t)})
}

// upsertPanel updates columns of the settings row, creating it with
// defaults first if needed.
func (s *Store) upsertPanel(ctx context.Context, set map[string]any) error {
	now := normTime(s.now())
	return mapDBError(s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		row := defaultPanelSettings()
		row.UpdatedAt = now
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		set["updated_at"] = now
		return tx.Model(&PanelSettings{}).Where("id = 1").Updates(set).Error
	}))
}

type panelMenuRow struct {
	ID       int64  `gorm:"column:id;primaryKey"`
	Position int    `gorm:"column:position"`
	Label    string `gorm:"column:label"`
	Action   string `gorm:"column:action"`
	Confirm  bool   `gorm:"column:confirm"`
	Roles    string `gorm:"column:roles"`
	Enabled  bool   `gorm:"column:enabled"`
}

func (panelMenuRow) TableName() string { return "panel_menu" }

// PanelMenu returns the menu in order, and whether it has been edited.
// Until it is, the defaults apply (with no IDs).
func (s *Store) PanelMenu(ctx context.Context) ([]menu.Item, bool, error) {
	p, err := s.GetPanelSettings(ctx)
	if err != nil {
		return nil, false, err
	}
	if !p.MenuEdited {
		items, err := menu.Validate(menu.Defaults())
		for i := range items {
			items[i].ID = int64(i + 1) // stable: defaults never change order
		}
		return items, false, err
	}
	var rows []panelMenuRow
	if err := s.db.WithContext(ctx).Order("position, id").Find(&rows).Error; err != nil {
		return nil, true, mapDBError(err)
	}
	items := make([]menu.Item, len(rows))
	for i, r := range rows {
		items[i] = menu.Item{ID: r.ID, Position: r.Position, Label: r.Label, Action: menu.Action(r.Action),
			Confirm: r.Confirm, Roles: r.Roles, Enabled: r.Enabled}
	}
	return items, true, nil
}

// ReplacePanelMenu saves an edited menu, already checked with
// menu.Validate, in one transaction.
func (s *Store) ReplacePanelMenu(ctx context.Context, items []menu.Item) error {
	if _, err := menu.Validate(items); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	if err := s.upsertPanel(ctx, map[string]any{}); err != nil {
		return err
	}
	return mapDBError(s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("DELETE FROM panel_menu").Error; err != nil {
			return err
		}
		for i, it := range items {
			row := panelMenuRow{Position: i + 1, Label: it.Label, Action: string(it.Action), Confirm: it.Confirm, Roles: it.Roles, Enabled: it.Enabled}
			if err := tx.Create(&row).Error; err != nil {
				return err
			}
		}
		return tx.Model(&PanelSettings{}).Where("id = 1").Updates(map[string]any{
			"menu_edited": true, "menu_rev": gorm.Expr("menu_rev + 1")}).Error
	}))
}

// ResetPanelMenu goes back to the default menu.
func (s *Store) ResetPanelMenu(ctx context.Context) error {
	if err := s.upsertPanel(ctx, map[string]any{"menu_edited": false, "menu_rev": gorm.Expr("menu_rev + 1")}); err != nil {
		return err
	}
	return mapDBError(s.db.WithContext(ctx).Exec("DELETE FROM panel_menu").Error)
}
