package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenFileAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkin-board.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s.SchemaVersion(ctx)
	if err != nil || v < 1 {
		t.Fatalf("SchemaVersion = %d, %v", v, err)
	}
	if _, err := s.SaveSettings(ctx, DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening must not re-run migrations (CREATE TABLE would fail) and
	// must keep the data.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	v2, _ := s2.SchemaVersion(ctx)
	if v2 != v {
		t.Errorf("version after reopen = %d, want %d", v2, v)
	}
	var n int64
	if err := s2.db.Table("settings").Count(&n).Error; err != nil || n != 1 {
		t.Fatalf("settings rows after reopen = %d, %v", n, err)
	}
}

func TestOpenPragmas(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	checks := map[string]string{"foreign_keys": "1", "journal_mode": "wal", "synchronous": "2"}
	for pragma, want := range checks {
		var got string
		if err := s.db.Raw("PRAGMA " + pragma).Scan(&got).Error; err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	if _, err := Open(""); err == nil {
		t.Error("empty path: expected error")
	}
	dir := filepath.Join(t.TempDir(), "missing", "dir")
	if _, err := Open(filepath.Join(dir, "x.db")); err == nil {
		t.Error("missing parent dir: expected error")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("Open created %s", dir)
	}
}

func TestOpenMemoryIsolated(t *testing.T) {
	a := newTestStore(t)
	b := newTestStore(t)
	if _, err := a.SaveSettings(ctx, DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := b.db.Table("settings").Count(&n).Error; err != nil || n != 0 {
		t.Fatalf("second memory store sees %d settings rows, %v", n, err)
	}
}

func TestMigrationFilesOrdered(t *testing.T) {
	files, err := migrationFiles()
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range files {
		if m.version != i+1 {
			t.Errorf("migration %d is %s; versions must be contiguous from 1", i, m.path)
		}
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s := newTestStore(t)
	bad := uint(999)
	err := s.db.Create(&LocalEntry{CPCode: "3", Bib: 1, TimeIn: t0, State: EntryQueued, BatchID: &bad, CreatedAt: t0}).Error
	if err == nil {
		t.Fatal("insert with a dangling batch_id succeeded; foreign keys are off")
	}
}

func TestDBSizeGrowsWithData(t *testing.T) {
	s := newTestStore(t)
	before, err := s.DBSize(ctx)
	if err != nil || before <= 0 {
		t.Fatalf("size = %d, %v", before, err)
	}
	for i := range 200 {
		if err := s.RecordBadReport(ctx, uint64(i+1), "K1CP", "AS5", strings.Repeat("x", 200), "bad", t0); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := s.DBSize(ctx)
	if after <= before {
		t.Fatalf("size %d -> %d", before, after)
	}
}
