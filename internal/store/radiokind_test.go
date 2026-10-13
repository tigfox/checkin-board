package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRadioIDsAboveGraywolfs(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	m := &RadioMessage{Direction: "out", Status: "queued", ToCall: "KD2DCM-1", PeerCall: "KD2DCM-1", Text: "x"}
	if err := s.RadioInsert(ctx, m); err != nil {
		t.Fatal(err)
	}
	if m.ID < RadioIDBase {
		t.Fatalf("radio id %d below %d", m.ID, RadioIDBase)
	}
}

func TestRadioKindChoice(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if k, err := s.RadioKind(ctx); err != nil || k != "" {
		t.Fatalf("fresh = %q %v", k, err)
	}
	if err := s.SetRadioKind(ctx, " "); err == nil {
		t.Fatal("empty kind saved")
	}
	if err := s.SetRadioKind(ctx, "uvk5"); err != nil {
		t.Fatal(err)
	}
	if k, _ := s.RadioKind(ctx); k != "uvk5" {
		t.Fatalf("kind = %q", k)
	}
}

func cursor(t *testing.T, s *Store) string {
	t.Helper()
	c, err := s.InboxCursor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// Each kind keeps its own inbox cursor across switches; the starting
// point is kept.
func TestActivateRadioKindSwapsCursors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	since := time.Date(2026, 10, 13, 7, 0, 0, 0, time.UTC)
	if err := s.EnsureInboxSince(ctx, since); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveInboxCursor(ctx, "gw-41"); err != nil {
		t.Fatal(err)
	}
	// An existing node's first start with kinds keeps its cursor.
	if changed, err := s.ActivateRadioKind(ctx, "graywolf"); err != nil || changed || cursor(t, s) != "gw-41" {
		t.Fatalf("first: %v %v %q", changed, err, cursor(t, s))
	}
	if k, _ := s.RadioKind(ctx); k != "graywolf" {
		t.Fatalf("kind = %q", k)
	}
	if changed, err := s.ActivateRadioKind(ctx, "uvk5"); err != nil || !changed || cursor(t, s) != "" {
		t.Fatalf("to uvk5: %v %v %q", changed, err, cursor(t, s))
	}
	if got, _ := s.InboxSince(ctx); !got.Equal(since) {
		t.Fatalf("since = %v", got)
	}
	_ = s.SaveInboxCursor(ctx, "7")
	if changed, _ := s.ActivateRadioKind(ctx, "uvk5"); changed {
		t.Fatal("same kind reported a change")
	}
	if _, err := s.ActivateRadioKind(ctx, "graywolf"); err != nil || cursor(t, s) != "gw-41" {
		t.Fatalf("back to graywolf: %v %q", err, cursor(t, s))
	}
	if _, err := s.ActivateRadioKind(ctx, "uvk5"); err != nil || cursor(t, s) != "7" {
		t.Fatalf("back to uvk5: %v %q", err, cursor(t, s))
	}
	// Re-reading from a time forgets every kind's cursor.
	if err := s.SetInboxSince(ctx, since); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateRadioKind(ctx, "graywolf"); err != nil || cursor(t, s) != "" {
		t.Fatalf("after re-read: %v %q", err, cursor(t, s))
	}
}

// A phase 17 radio node's rows (small ids) move above RadioIDBase with
// every reference when it upgrades to migration 0010.
func TestMigration0010MovesRadioRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cb.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Back to schema 9 with a radio row 5, bound to a batch and recorded.
	for _, q := range []string{
		"DROP TABLE node_radio", "DROP TABLE inbox_cursors", "DELETE FROM schema_migrations WHERE version = 10",
		"UPDATE sqlite_sequence SET seq = 5 WHERE name = 'radio_messages'",
		`INSERT INTO radio_messages (id, direction, status, created_at, upd_seq) VALUES (5, 'out', 'acked', '2026-10-12 13:00:00', 1)`,
		"INSERT INTO gw_rows (gw_message_id, kind, created_at) VALUES (5, 'batch', '2026-10-12 13:00:00')",
		`INSERT INTO batches (cp_code, seq, text, state, gw_message_id, next_tx_at, created_at)
			VALUES ('AS1', 1, 'RC1 R AS1 1 x', 'acked', 5, '2026-10-12 13:00:00', '2026-10-12 13:00:00')`,
	} {
		if err := s.db.Exec(q).Error; err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	_ = s.Close()
	if s, err = Open(path); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	moved := RadioIDBase + 5
	if _, err := s.RadioGet(ctx, moved); err != nil {
		t.Fatalf("radio row not moved: %v", err)
	}
	if known, _ := s.KnownGWRow(ctx, moved); !known {
		t.Fatal("gw_rows not moved")
	}
	var bound uint64
	if err := s.db.Raw("SELECT gw_message_id FROM batches WHERE cp_code = 'AS1'").Scan(&bound).Error; err != nil || bound != moved {
		t.Fatalf("batch bound to %d (%v)", bound, err)
	}
	m := &RadioMessage{Direction: "out", Status: "queued", ToCall: "KD2DCM-1", PeerCall: "KD2DCM-1", Text: "next"}
	if err := s.RadioInsert(ctx, m); err != nil || m.ID <= moved {
		t.Fatalf("next id %d (%v)", m.ID, err)
	}
}
