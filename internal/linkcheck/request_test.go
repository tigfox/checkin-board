package linkcheck

import (
	"context"
	"errors"
	"testing"
	"time"

	"checkin-board/internal/store"
)

var ctx = context.Background()

var t0 = time.Date(2026, 10, 10, 6, 0, 0, 0, time.UTC)

func openStore(t *testing.T, cfg store.Settings) *store.Store {
	t.Helper()
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	return st
}

func cpSettings(state string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceName, c.RaceState = store.RoleCheckpoint, "AS5", "N0CALL-10", "Ridge", state
	return c
}

func hqSettings(state string) store.Settings {
	c := store.DefaultSettings()
	c.Role, c.HQLocalCodes, c.RaceName, c.RaceState = store.RoleHQ, "START,FIN", "Ridge", state
	return c
}

func TestRequestCheckpointProbesHQ(t *testing.T) {
	st := openStore(t, cpSettings(store.RaceSetup))
	c, err := Request(ctx, st, Req{Source: "admin"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if c.PeerCall != "N0CALL-10" || c.StationCode != "AS5" || c.Count != DefaultCount || c.SpacingSec != int(DefaultSpacing/time.Second) {
		t.Fatalf("check = %+v", c)
	}
	if _, err := Request(ctx, st, Req{}, t0.Add(time.Minute)); !errors.Is(err, ErrBusy) {
		t.Fatalf("second while one is pending: %v", err)
	}
}

func TestRequestRules(t *testing.T) {
	st := openStore(t, cpSettings(store.RaceActive))
	if _, err := Request(ctx, st, Req{}, t0); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("active without confirm: %v", err)
	}
	if _, err := Request(ctx, st, Req{Count: MaxCount + 1, Confirm: true}, t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too many probes: %v", err)
	}
	c, err := Request(ctx, st, Req{Confirm: true, Count: 3}, t0)
	if err != nil || c.Count != 3 {
		t.Fatalf("confirmed: %+v, %v", c, err)
	}
	fin := t0.Add(time.Minute)
	c.State, c.FinishedAt = store.LinkCheckDone, &fin
	if err := st.UpdateLinkCheck(ctx, c); err != nil {
		t.Fatal(err)
	}
	var soon *TooSoonError
	if _, err := Request(ctx, st, Req{Confirm: true}, t0.Add(90*time.Second)); !errors.As(err, &soon) || soon.RetryAfter != 30*time.Second {
		t.Fatalf("within 2 min: %v", err)
	}
	if _, err := Request(ctx, st, Req{Confirm: true}, t0.Add(MinInterval)); err != nil {
		t.Fatalf("after 2 min: %v", err)
	}

	for _, state := range []string{store.RaceComplete, store.RaceSecured, store.RaceCheckingIn, store.RaceCheckedIn} {
		st := openStore(t, cpSettings(state))
		if _, err := Request(ctx, st, Req{Confirm: true}, t0); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s: %v", state, err)
		}
	}
	none := openStore(t, store.DefaultSettings())
	if _, err := Request(ctx, none, Req{}, t0); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("no role: %v", err)
	}
}

func TestRequestHQNeedsTarget(t *testing.T) {
	st := openStore(t, hqSettings(store.RaceSetup))
	if _, err := Request(ctx, st, Req{}, t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("HQ without --to: %v", err)
	}
	if _, err := Request(ctx, st, Req{To: "not a call"}, t0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad call: %v", err)
	}
	c, err := Request(ctx, st, Req{To: "n0call-1"}, t0)
	if err != nil || c.PeerCall != "N0CALL-1" || c.StationCode != HQCode {
		t.Fatalf("HQ check = %+v, %v", c, err)
	}
}
