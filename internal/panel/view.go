// Package panel is the node panel (spec 8.4): the e-ink status screen
// and the two-button menu. The app serves a View over its local hook;
// the panel process renders it and reports button actions back.
package panel

import (
	"time"

	"checkin-board/internal/panel/menu"
)

// View is everything the panel needs from the app (GET /api/hook/panel).
type View struct {
	Status   Status     `json:"status"`
	Menu     []MenuItem `json:"menu"`
	Settings Settings   `json:"settings"`
	// Targets are the checkpoints HQ can link-check from the panel.
	Targets []Target `json:"targets,omitempty"`
	// FullAllowedAt is the earliest next full refresh (3-minute floor,
	// remembered by the app across panel restarts). Nil: any time.
	FullAllowedAt *time.Time `json:"full_allowed_at,omitempty"`
	// RefreshSeq changes when an admin asks for a refresh.
	RefreshSeq int64 `json:"refresh_seq"`
	// TestPattern names a controller to show a test pattern with;
	// TestPatternSeq changes with each request.
	TestPattern    string `json:"test_pattern,omitempty"`
	TestPatternSeq int64  `json:"test_pattern_seq"`
	// Boot identifies the app process: the request sequences above start
	// over when it restarts, so the panel re-reads them then.
	Boot int64 `json:"boot"`
	// MenuRev changes on every menu edit; actions carry it back.
	MenuRev int64 `json:"menu_rev"`
}

// Settings are the admin's panel settings.
type Settings struct {
	Enabled    bool   `json:"enabled"`
	Controller string `json:"controller"` // "": not known
	// DetectPending asks the panel to run the detection wizard (unknown
	// controller, and the wizard hasn't given up since the last request).
	DetectPending bool `json:"detect_pending"`
	RefreshMin    int  `json:"refresh_min"`
	Rotation      int  `json:"rotation"`
}

// MenuItem is a menu entry and whether it can be used now.
type MenuItem struct {
	menu.Item
	Available bool `json:"available"`
}

// Target is a checkpoint HQ can probe.
type Target struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Call string `json:"call"`
}

// Status is the node's state for the status screen.
type Status struct {
	Role       string `json:"role"`
	RaceName   string `json:"race_name"`
	RaceState  string `json:"race_state"`
	StateLabel string `json:"state_label"`
	Station    string `json:"station"` // tactical name
	CPCode     string `json:"cp_code,omitempty"`
	// Checkpoint delivery.
	Unconfirmed   int        `json:"unconfirmed"`
	LastHQContact *time.Time `json:"last_hq_contact,omitempty"`
	// graywolf.
	GraywolfOK      bool     `json:"graywolf_ok"`
	GraywolfProblem string   `json:"graywolf_problem,omitempty"`
	LastLink        *Link    `json:"last_link,omitempty"`
	HQ              *HQ      `json:"hq,omitempty"`
	Warnings        []string `json:"warnings,omitempty"`
	// Port is the web UI's port, for the address on the screen.
	Port int       `json:"port"`
	Now  time.Time `json:"now"`
}

// Link is the latest link check.
type Link struct {
	Verdict   string    `json:"verdict"`
	At        time.Time `json:"at"`
	Peer      string    `json:"peer"`
	Uplink    int       `json:"uplink"`
	RoundTrip int       `json:"round_trip"`
	Count     int       `json:"count"`
}

// HQ summarizes the checkpoints at HQ.
type HQ struct {
	Listed int `json:"listed"`
	Heard  int `json:"heard"`
	Closed int `json:"closed"`
	Gaps   int `json:"gaps"`
}

// ActionRequest runs a menu item (POST /api/hook/panel/actions/{id}).
type ActionRequest struct {
	// Action is what the panel thinks the item does: if the menu was
	// edited since its last poll, the app refuses rather than run a
	// different item.
	Action  menu.Action `json:"action"`
	MenuRev int64       `json:"menu_rev"`
	To      string      `json:"to,omitempty"` // HQ link check: the checkpoint's call
}

// ActionResult is the line the panel shows afterwards.
type ActionResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}
