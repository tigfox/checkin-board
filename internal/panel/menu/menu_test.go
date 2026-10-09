package menu

import (
	"errors"
	"strings"
	"testing"
)

func TestDefaultsAreValidForEachRole(t *testing.T) {
	items, err := Validate(Defaults())
	if err != nil {
		t.Fatal(err)
	}
	labels := func(role string) []string {
		var out []string
		for _, it := range items {
			if it.ShowsFor(role) {
				out = append(out, it.Label)
			}
		}
		return out
	}
	cp := strings.Join(labels("checkpoint"), "|")
	for _, want := range []string{"Open checkpoint", "Close checkpoint", "Secure for travel", "HQ check-in", "Run link check"} {
		if !strings.Contains(cp, want) {
			t.Errorf("checkpoint menu %q lacks %q", cp, want)
		}
	}
	hq := strings.Join(labels("hq"), "|")
	if strings.Contains(hq, "checkpoint") || !strings.Contains(hq, "Open race") {
		t.Errorf("HQ menu = %q", hq)
	}
}

func TestValidate(t *testing.T) {
	ok := Item{Label: "Open checkpoint", Action: StartRace, Roles: RolesCheckpoint, Enabled: true}
	got, err := Validate([]Item{ok})
	if err != nil || !got[0].Confirm || got[0].Position != 1 {
		t.Fatalf("lifecycle item = %+v, %v (confirm must be forced on)", got, err)
	}
	lc, _ := Validate([]Item{{Label: "Link", Action: LinkCheck, Roles: RolesBoth, Confirm: false}})
	if !lc[0].Confirm {
		t.Fatal("link check confirm not forced (it transmits)")
	}
	for name, bad := range map[string]Item{
		"unknown action":      {Label: "Reset", Action: "reset", Roles: RolesBoth},
		"empty label":         {Label: "", Action: Status, Roles: RolesBoth},
		"long label":          {Label: strings.Repeat("x", MaxLabel+1), Action: Status, Roles: RolesBoth},
		"non-ascii label":     {Label: "Café", Action: Status, Roles: RolesBoth},
		"control char":        {Label: "a\tb", Action: Status, Roles: RolesBoth},
		"bad roles":           {Label: "x", Action: Status, Roles: "admin"},
		"secure at HQ":        {Label: "x", Action: Secure, Roles: RolesHQ},
		"check-in everywhere": {Label: "x", Action: CheckIn, Roles: RolesBoth},
	} {
		if _, err := Validate([]Item{bad}); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	many := make([]Item, MaxItems+1)
	for i := range many {
		many[i] = Item{Label: "Status", Action: Status, Roles: RolesBoth}
	}
	if _, err := Validate(many); !errors.Is(err, ErrInvalid) {
		t.Error("too many items accepted")
	}
}

func TestAvailableFollowsRoleAndState(t *testing.T) {
	item := func(a Action, roles string) Item { return Item{Label: "x", Action: a, Roles: roles, Enabled: true} }
	cases := []struct {
		it          Item
		role, state string
		want        bool
	}{
		{item(StartRace, RolesBoth), "checkpoint", "setup", true},
		{item(StartRace, RolesBoth), "checkpoint", "active", false},
		{item(CompleteRace, RolesBoth), "checkpoint", "active", true},
		{item(Secure, RolesCheckpoint), "checkpoint", "complete", true},
		{item(Secure, RolesCheckpoint), "checkpoint", "setup", false},
		{item(CheckIn, RolesCheckpoint), "checkpoint", "secured", true},
		{item(CheckIn, RolesCheckpoint), "checkpoint", "checked_in", false},
		{item(LinkCheck, RolesBoth), "hq", "active", true},
		{item(LinkCheck, RolesBoth), "hq", "complete", false},
		{item(Status, RolesBoth), "hq", "checked_in", true},
		{item(Status, RolesCheckpoint), "hq", "setup", false},
		{item(StartRace, RolesBoth), "", "setup", false},
		{item(ShowNetwork, RolesBoth), "", "setup", true}, // a node being set up
		{item(ShowNetwork, RolesHQ), "", "setup", false},
		{Item{Label: "x", Action: Status, Roles: RolesBoth}, "hq", "setup", false}, // disabled
	}
	for i, c := range cases {
		if got := c.it.Available(c.role, c.state); got != c.want {
			t.Errorf("%d: %s for %s in %s = %v, want %v", i, c.it.Action, c.role, c.state, got, c.want)
		}
	}
}

func TestLocalActions(t *testing.T) {
	for _, a := range []Action{Status, ShowNetwork, ShowLink, Refresh} {
		if !a.Local() {
			t.Errorf("%s should be handled on the panel", a)
		}
	}
	for _, a := range []Action{LinkCheck, StartRace, CompleteRace, Secure, CheckIn} {
		if a.Local() {
			t.Errorf("%s should run in the app", a)
		}
	}
}
