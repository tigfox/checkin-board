package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"checkin-board/internal/config"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/store"
)

func TestRadioHold(t *testing.T) {
	st := radiobackend.State{}
	hold := radioHold(func() radiobackend.State { return st })
	if hold() != "radio not connected" {
		t.Fatalf("disconnected: %q", hold())
	}
	st.Connected = true
	if hold() != "" {
		t.Fatalf("connected: %q", hold())
	}
}

func TestChooseRadio(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.DiscardHandler)
	st, err := store.Open(filepath.Join(t.TempDir(), "cb.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	gw := config.Config{GraywolfUser: "checkin-board", GraywolfPassword: "pw"}

	// Nothing chosen: CB_BACKEND, else the default.
	if k, err := chooseRadio(ctx, config.Config{Backend: "uvk5"}, st, logger); err != nil || k.ID != "uvk5" {
		t.Fatalf("CB_BACKEND: %+v %v", k, err)
	}
	if _, err := st.ActivateRadioKind(ctx, "graywolf"); err != nil { // as if it had run with graywolf
		t.Fatal(err)
	}
	if err := st.SetRadioKind(ctx, ""); err == nil {
		t.Fatal("empty kind saved")
	}
	// The Station page's choice wins over CB_BACKEND.
	if err := st.SetRadioKind(ctx, "uvk5"); err != nil {
		t.Fatal(err)
	}
	gwDefault := gw
	gwDefault.Backend = "graywolf"
	if k, err := chooseRadio(ctx, gwDefault, st, logger); err != nil || k.ID != "uvk5" {
		t.Fatalf("chosen: %+v %v", k, err)
	}
	// graywolf without its login: the app stays up on the UV-K5, so the
	// Station page can be reached.
	if err := st.SetRadioKind(ctx, "graywolf"); err != nil {
		t.Fatal(err)
	}
	if k, err := chooseRadio(ctx, config.Config{}, st, logger); err != nil || k.ID != "uvk5" {
		t.Fatalf("graywolf without login: %+v %v", k, err)
	}
	// A kind this build doesn't know (a newer build saved it): the default.
	if err := st.SetRadioKind(ctx, "future-radio"); err != nil {
		t.Fatal(err)
	}
	if k, err := chooseRadio(ctx, gw, st, logger); err != nil || k.ID != "graywolf" {
		t.Fatalf("unknown kind: %+v %v", k, err)
	}
}
