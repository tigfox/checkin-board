// Package menu is the node panel's button menu (spec 8.4): the fixed
// allowlist of actions, the per-role defaults, validation of an edited
// menu, and which items are available in the node's role and state.
// It has no dependencies, so the app and the panel share it.
package menu

import (
	"errors"
	"fmt"
)

// Action is one allowlisted panel command.
type Action string

// The allowlist. Nothing that loses data or changes graywolf (Reset,
// cleanup, passwords, callsign) is ever a panel action.
const (
	Status       Action = "status"
	LinkCheck    Action = "link_check"
	StartRace    Action = "start_race"
	CompleteRace Action = "complete_race"
	Secure       Action = "secure"
	CheckIn      Action = "check_in"
	ShowNetwork  Action = "show_network"
	ShowLink     Action = "show_link"
	Refresh      Action = "refresh"
)

// Who an item shows for.
const (
	RolesBoth       = "both"
	RolesCheckpoint = "checkpoint"
	RolesHQ         = "hq"
)

// Limits on an edited menu.
const (
	MaxItems = 16
	MaxLabel = 20
)

// ErrInvalid: an edited menu breaks a rule.
var ErrInvalid = errors.New("menu: invalid")

type actionInfo struct {
	local     bool   // handled on the panel, no call to the app
	lifecycle bool   // changes the race state: always confirmed
	transmits bool   // uses airtime: always confirmed
	roles     string // the roles it can apply to
	// states lists the race states it is available in (nil: any).
	states []string
}

var actions = map[Action]actionInfo{
	Status:       {local: true, roles: RolesBoth},
	ShowNetwork:  {local: true, roles: RolesBoth},
	ShowLink:     {local: true, roles: RolesBoth},
	Refresh:      {local: true, roles: RolesBoth},
	LinkCheck:    {roles: RolesBoth, transmits: true, states: []string{"setup", "active"}},
	StartRace:    {lifecycle: true, roles: RolesBoth, states: []string{"setup"}},
	CompleteRace: {lifecycle: true, roles: RolesBoth, states: []string{"active"}},
	Secure:       {lifecycle: true, roles: RolesCheckpoint, states: []string{"active", "complete", "checking_in"}},
	CheckIn:      {lifecycle: true, roles: RolesCheckpoint, states: []string{"secured", "complete"}},
}

// Actions lists the allowlist in a stable order (for the editor).
func Actions() []Action {
	return []Action{Status, LinkCheck, StartRace, CompleteRace, Secure, CheckIn, ShowLink, ShowNetwork, Refresh}
}

// Known reports whether a is on the allowlist.
func (a Action) Known() bool { _, ok := actions[a]; return ok }

// Local reports whether the panel handles a itself.
func (a Action) Local() bool { return actions[a].local }

// Lifecycle reports whether a changes the race state.
func (a Action) Lifecycle() bool { return actions[a].lifecycle }

// AlwaysConfirm reports whether a item for a must ask first: anything
// that changes the race state or transmits.
func (a Action) AlwaysConfirm() bool { return actions[a].lifecycle || actions[a].transmits }

// Roles is the roles a can apply to.
func (a Action) Roles() string { return actions[a].roles }

// Item is one menu entry.
type Item struct {
	ID       int64  `json:"id"`
	Position int    `json:"position"`
	Label    string `json:"label"`
	Action   Action `json:"action"`
	Confirm  bool   `json:"confirm"`
	Roles    string `json:"roles"`
	Enabled  bool   `json:"enabled"`
}

// ShowsFor reports whether the item is meant for role.
func (it Item) ShowsFor(role string) bool {
	return role != "" && (it.Roles == RolesBoth || it.Roles == role)
}

// Available reports whether the item can be used now: enabled, meant
// for this role, and its action allowed in this race state. A node with
// no role yet still gets the panel-only items meant for both roles
// (status, network…), which help while setting it up.
func (it Item) Available(role, state string) bool {
	info, ok := actions[it.Action]
	if !ok || !it.Enabled {
		return false
	}
	if role == "" {
		return info.local && it.Roles == RolesBoth
	}
	if !it.ShowsFor(role) {
		return false
	}
	if info.roles != RolesBoth && info.roles != role {
		return false
	}
	if info.states == nil {
		return true
	}
	for _, s := range info.states {
		if s == state {
			return true
		}
	}
	return false
}

// Defaults is the menu a node starts with: a checkpoint's runs
// Open → Close → Secure → HQ check-in (4.7).
func Defaults() []Item {
	return []Item{
		{Label: "Status", Action: Status, Roles: RolesBoth, Enabled: true},
		{Label: "Run link check", Action: LinkCheck, Roles: RolesBoth, Confirm: true, Enabled: true},
		{Label: "Open checkpoint", Action: StartRace, Roles: RolesCheckpoint, Enabled: true},
		{Label: "Close checkpoint", Action: CompleteRace, Roles: RolesCheckpoint, Enabled: true},
		{Label: "Secure for travel", Action: Secure, Roles: RolesCheckpoint, Enabled: true},
		{Label: "HQ check-in", Action: CheckIn, Roles: RolesCheckpoint, Enabled: true},
		{Label: "Open race", Action: StartRace, Roles: RolesHQ, Enabled: true},
		{Label: "Close race", Action: CompleteRace, Roles: RolesHQ, Enabled: true},
		{Label: "Last link check", Action: ShowLink, Roles: RolesBoth, Enabled: true},
		{Label: "Network", Action: ShowNetwork, Roles: RolesBoth, Enabled: true},
		{Label: "Refresh screen", Action: Refresh, Roles: RolesBoth, Enabled: true},
	}
}

// Validate checks an edited menu and returns it normalized: positions
// renumbered in order and confirm forced on for lifecycle actions and
// anything that transmits.
// Labels are printable ASCII, because the panel's font is.
func Validate(items []Item) ([]Item, error) {
	if len(items) > MaxItems {
		return nil, fmt.Errorf("%w: at most %d items", ErrInvalid, MaxItems)
	}
	out := make([]Item, len(items))
	for i, it := range items {
		info, ok := actions[it.Action]
		switch {
		case !ok:
			return nil, fmt.Errorf("%w: item %d: %q is not a panel action", ErrInvalid, i+1, it.Action)
		case it.Label == "" || len(it.Label) > MaxLabel:
			return nil, fmt.Errorf("%w: item %d: the label must be 1-%d characters", ErrInvalid, i+1, MaxLabel)
		case !printableASCII(it.Label):
			return nil, fmt.Errorf("%w: item %d: the label must be plain letters, digits and punctuation (the display has no accents)", ErrInvalid, i+1)
		case it.Roles != RolesBoth && it.Roles != RolesCheckpoint && it.Roles != RolesHQ:
			return nil, fmt.Errorf("%w: item %d: roles must be both, checkpoint or hq", ErrInvalid, i+1)
		case info.roles != RolesBoth && it.Roles != info.roles:
			return nil, fmt.Errorf("%w: item %d: %q only applies to a %s", ErrInvalid, i+1, it.Action, info.roles)
		}
		it.Position = i + 1
		if info.lifecycle || info.transmits {
			it.Confirm = true
		}
		out[i] = it
	}
	return out, nil
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
