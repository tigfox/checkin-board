package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"checkin-board/internal/auth"
	"checkin-board/internal/store"
)

func TestResetAdminPassword(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "checkin-board.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	a := auth.New(st, nil, 4)
	code, _ := a.SetupCode(ctx)
	if _, err := a.Setup(ctx, code, "forgotten admin password"); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	var out bytes.Buffer
	if err := resetAdminPassword(ctx, db, strings.NewReader("recovered password\n"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Race data was not touched") {
		t.Fatalf("output = %q", out.String())
	}
	st, _ = store.Open(db)
	defer st.Close()
	if _, err := auth.New(st, nil, 4).Login(ctx, store.RoleAdmin, "recovered password", "cli"); err != nil {
		t.Fatalf("login with the new password: %v", err)
	}
	if err := resetAdminPassword(ctx, db, strings.NewReader("short\n"), &out); err == nil {
		t.Fatal("weak password accepted")
	}
}

func TestPrintVersion(t *testing.T) {
	old := version
	version = "1.2.3"
	t.Cleanup(func() { version = old })
	var out bytes.Buffer
	printVersion(&out)
	for _, want := range []string{"checkin-board 1.2.3", "graywolf " + testedGraywolfVersion, "go1."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("version output %q lacks %q", out.String(), want)
		}
	}
}
