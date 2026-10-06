package peers

import (
	"context"
	"errors"
	"testing"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
)

var ctx = context.Background()

func setup(t *testing.T) (*gwfake.Station, *store.Store) {
	t.Helper()
	s, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return gwfake.New("K1CP"), s
}

func TestEnsureTurnsOffRetriesAndBacksUpOriginal(t *testing.T) {
	gw, st := setup(t)
	// The operator had routed N0HQ RF-only.
	_, _ = gw.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ", graywolf.ConversationPrefs{SendPath: "rf_only", WaitForAck: true})

	if err := Ensure(ctx, gw, st, []string{"N0HQ"}); err != nil {
		t.Fatal(err)
	}
	p, _ := gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ")
	if p.WaitForAck || p.SendPath != "rf_only" {
		t.Fatalf("prefs = %+v, want retries off and routing kept", p)
	}
	backups, _ := st.ListPeerPrefs(ctx)
	if len(backups) != 1 || !backups[0].WaitForAck || backups[0].SendPath != "rf_only" {
		t.Fatalf("backups = %+v", backups)
	}

	// Running again (e.g. after a restart) keeps the true original.
	if err := Ensure(ctx, gw, st, []string{"N0HQ"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := st.ListPeerPrefs(ctx); !b[0].WaitForAck {
		t.Fatalf("backup overwritten: %+v", b)
	}

	// Restore puts the operator's settings back and clears the backup.
	if err := Restore(ctx, gw, st); err != nil {
		t.Fatal(err)
	}
	p, _ = gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ")
	if !p.WaitForAck || p.SendPath != "rf_only" {
		t.Fatalf("restored prefs = %+v", p)
	}
	if b, _ := st.ListPeerPrefs(ctx); len(b) != 0 {
		t.Fatalf("backups after restore = %+v", b)
	}
}

func TestRestoreDefaultsLeavesNoOverride(t *testing.T) {
	gw, st := setup(t)
	if err := Ensure(ctx, gw, st, []string{"N0HQ", "K2CP-7"}); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, gw, st); err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{"N0HQ", "K2CP-7"} {
		if gw.HasPrefsOverride(graywolf.ThreadKindDM, call) {
			t.Errorf("%s: restore left an override in graywolf", call)
		}
	}
}

func TestEnsureSkipsBlankAndRejectsBadCalls(t *testing.T) {
	gw, st := setup(t)
	if err := Ensure(ctx, gw, st, []string{"", "N0HQ"}); err != nil {
		t.Fatal(err)
	}
	if err := Ensure(ctx, gw, st, []string{"not a call"}); err == nil {
		t.Fatal("expected error for an invalid callsign")
	}
}

func TestErrorsAreReturnedAndBackupKept(t *testing.T) {
	gw, st := setup(t)
	gw.FailPrefs(errors.New("graywolf down"))
	if err := Ensure(ctx, gw, st, []string{"N0HQ"}); err == nil {
		t.Fatal("expected error")
	}
	gw.FailPrefs(nil)
	if err := Ensure(ctx, gw, st, []string{"N0HQ"}); err != nil {
		t.Fatal(err)
	}
	gw.FailPrefs(errors.New("graywolf down"))
	if err := Restore(ctx, gw, st); err == nil {
		t.Fatal("expected restore error")
	}
	if b, _ := st.ListPeerPrefs(ctx); len(b) != 1 {
		t.Fatalf("backup dropped although the restore failed: %+v", b)
	}
}
