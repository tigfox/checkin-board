package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRadioMessages(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	a := &RadioMessage{Direction: "out", Status: "queued", ToCall: "KD2DCM-1", PeerCall: "KD2DCM-1", Text: "one"}
	b := &RadioMessage{Direction: "in", Status: "received", FromCall: "KD2DCM-1", PeerCall: "KD2DCM-1", Text: "two", MsgID: "050", Unread: true}
	for _, m := range []*RadioMessage{a, b} {
		if err := s.RadioInsert(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	if a.ID == 0 || b.ID == a.ID || b.UpdSeq <= a.UpdSeq || a.CreatedAt.IsZero() {
		t.Fatalf("ids/seqs: %+v %+v", a, b)
	}
	// An update moves a row to the end of the feed; a no-op doesn't.
	up, err := s.RadioUpdate(ctx, a.ID, func(m *RadioMessage) bool { m.Status, m.MsgID = "tx_submitted", "07"; return true })
	if err != nil || up.UpdSeq <= b.UpdSeq {
		t.Fatalf("update: %+v %v", up, err)
	}
	same, _ := s.RadioUpdate(ctx, a.ID, func(*RadioMessage) bool { return false })
	if same.UpdSeq != up.UpdSeq {
		t.Fatal("a no-op update moved the row")
	}
	ch, err := s.RadioChanges(ctx, 0, 0)
	if err != nil || len(ch) != 2 || ch[0].ID != b.ID || ch[1].ID != a.ID {
		t.Fatalf("changes = %+v %v", ch, err)
	}
	if ch, _ := s.RadioChanges(ctx, up.UpdSeq, 0); len(ch) != 0 {
		t.Fatalf("after the last: %+v", ch)
	}
	if ch, _ := s.RadioChanges(ctx, 0, 1); len(ch) != 1 {
		t.Fatalf("limit: %+v", ch)
	}
	out, _ := s.RadioOutbound(ctx, "tx_submitted", "sent_rf")
	if len(out) != 1 || out[0].ID != a.ID {
		t.Fatalf("outbound = %+v", out)
	}
	m, err := s.RadioMatchAck(ctx, "kd2dcm-1", "07", time.Now().Add(-time.Hour), "tx_submitted", "sent_rf")
	if err != nil || m.ID != a.ID {
		t.Fatalf("match = %+v %v", m, err)
	}
	if _, err := s.RadioMatchAck(ctx, "KD2DCM-1", "08", time.Now().Add(-time.Hour), "sent_rf", "tx_submitted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no match: %v", err)
	}
	if _, err := s.RadioMatchAck(ctx, "KD2DCM-1", "07", time.Now().Add(time.Hour), "sent_rf", "tx_submitted"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("too old: %v", err)
	}
	if got, err := s.RadioGet(ctx, b.ID); err != nil || got.Text != "two" || !got.Unread {
		t.Fatalf("get = %+v %v", got, err)
	}
	if err := s.RadioDelete(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RadioGet(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted: %v", err)
	}
	if err := s.RadioDelete(ctx, b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if _, err := s.RadioUpdate(ctx, 999, func(*RadioMessage) bool { return true }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing: %v", err)
	}
}

// Times from a node's local clock (the Pis run on local time, not UTC)
// are stored as UTC, so the ack window still matches: SQLite compares
// the stored text. Bench, 2026-10-13: every SENT line and ack missed.
func TestRadioMatchAckLocalTime(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	edt := time.FixedZone("EDT", -4*3600)
	m := &RadioMessage{Direction: "out", Status: "queued", ToCall: "KD2DCM-4", PeerCall: "KD2DCM-4", Text: "x",
		CreatedAt: time.Date(2026, 10, 13, 15, 37, 55, 123, edt)}
	if err := s.RadioInsert(ctx, m); err != nil {
		t.Fatal(err)
	}
	submitted := time.Date(2026, 10, 13, 15, 38, 0, 456, edt)
	if _, err := s.RadioUpdate(ctx, m.ID, func(r *RadioMessage) bool {
		r.Status, r.MsgID, r.CreatedAt, r.SentAt = "tx_submitted", "32", submitted, &submitted
		return true
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RadioMatchAck(ctx, "KD2DCM-4", "32", submitted.Add(-time.Minute), "tx_submitted")
	if err != nil || got.ID != m.ID {
		t.Fatalf("match = %+v, %v", got, err)
	}
	if !got.CreatedAt.Equal(submitted.Truncate(time.Second)) || got.CreatedAt.Location() != time.UTC {
		t.Fatalf("created_at = %v", got.CreatedAt)
	}
}
