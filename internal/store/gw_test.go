package store

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"checkin-board/internal/wire"
)

func TestGWRows(t *testing.T) {
	s := newTestStore(t)
	isNew, err := s.RecordGWRow(ctx, 10, GWRowHeartbeat)
	if err != nil || !isNew {
		t.Fatalf("first record = %v, %v", isNew, err)
	}
	if isNew, err := s.RecordGWRow(ctx, 10, GWRowHeartbeat); err != nil || isNew {
		t.Fatalf("second record = %v, %v; want existing", isNew, err)
	}
	if _, err := s.RecordGWRow(ctx, 0, GWRowGap); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("zero id err = %v", err)
	}
	if _, err := s.RecordGWRow(ctx, 11, "mystery"); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad kind err = %v", err)
	}
	if known, _ := s.KnownGWRow(ctx, 10); !known {
		t.Error("KnownGWRow(10) = false")
	}
	if known, _ := s.KnownGWRow(ctx, 12); known {
		t.Error("KnownGWRow(12) = true")
	}

	_, _ = s.RecordGWRow(ctx, 12, GWRowInbound)
	if err := s.MarkGWRowDeleted(ctx, 10, t0); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkGWRowDeleted(ctx, 99, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing row err = %v", err)
	}
	rows, err := s.ListGWRows(ctx)
	if err != nil || len(rows) != 1 || rows[0].GWMessageID != 12 || rows[0].Kind != GWRowInbound {
		t.Fatalf("ListGWRows = %+v, %v; want only undeleted row 12", rows, err)
	}
}

func TestIngestReportRecordsInboxRowAtomically(t *testing.T) {
	s := newTestStore(t)
	msg, err := wire.Decode("RC1 R AS5 1 @1300 101/05")
	if err != nil {
		t.Fatal(err)
	}
	rep := msg.(*wire.Report)
	if _, err := s.IngestReport(ctx, rep, "K1CP", 77, at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if known, _ := s.KnownGWRow(ctx, 77); !known {
		t.Fatal("ingested row not recorded")
	}
	var rb ReceivedBatch
	if err := s.db.First(&rb).Error; err != nil || rb.GWMessageID == nil || *rb.GWMessageID != 77 {
		t.Fatalf("received batch = %+v, %v", rb, err)
	}
	// The same batch via a different graywolf row (resent after graywolf's
	// dedup window) is a duplicate but its row is still recorded.
	res, err := s.IngestReport(ctx, rep, "K1CP", 78, at(10*time.Minute))
	if err != nil || !res.Duplicate {
		t.Fatalf("second copy = %+v, %v", res, err)
	}
	if known, _ := s.KnownGWRow(ctx, 78); !known {
		t.Fatal("duplicate's row not recorded")
	}
}

func TestInboxCursor(t *testing.T) {
	s := newTestStore(t)
	if c, err := s.InboxCursor(ctx); err != nil || c != "" {
		t.Fatalf("initial cursor = %q, %v", c, err)
	}
	for _, want := range []string{"abc", "def"} {
		if err := s.SaveInboxCursor(ctx, want); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.InboxCursor(ctx); got != want {
			t.Fatalf("cursor = %q, want %q", got, want)
		}
	}
}

func TestBadReports(t *testing.T) {
	s := newTestStore(t)
	if err := s.CreateCheckpoint(ctx, &Checkpoint{Code: "AS5", Name: "Aid 5"}); err != nil {
		t.Fatal(err)
	}
	long := "RC1 R AS5 " + strings.Repeat("x", 400)
	if err := s.RecordBadReport(ctx, 5, "K1CP", "AS5", long, "bad entry", t0); err != nil {
		t.Fatal(err)
	}
	// Same graywolf row again: no double count.
	if err := s.RecordBadReport(ctx, 5, "K1CP", "AS5", long, "bad entry", t0); err != nil {
		t.Fatal(err)
	}
	// A well-formed but unknown code (garbage or spoofed) is kept but
	// must not create a phantom checkpoint.
	if err := s.RecordBadReport(ctx, 6, "N0BAD", "ZZZ", "RC1 ???", "garbage", at(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordBadReport(ctx, 0, "N0BAD", "", "x", "y", t0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("zero id err = %v", err)
	}
	got, err := s.ListBadReports(ctx, 10)
	if err != nil || len(got) != 2 || got[0].GWMessageID != 6 {
		t.Fatalf("ListBadReports = %+v, %v", got, err)
	}
	if len(got[1].Text) != maxBadReportText {
		t.Errorf("text length = %d, want truncated to %d", len(got[1].Text), maxBadReportText)
	}
	sts, _ := s.ListStatuses(ctx)
	if len(sts) != 1 || sts[0].CPCode != "AS5" || sts[0].BadReports != 1 {
		t.Fatalf("statuses = %+v", sts)
	}
	// Both rows are recorded: the inbox reader skips them and cleanup
	// deletes them.
	for _, id := range []uint64{5, 6} {
		if known, _ := s.KnownGWRow(ctx, id); !known {
			t.Errorf("bad report row %d not recorded in gw_rows", id)
		}
	}
}

func TestTruncateUTF8(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abc", 5, "abc"},
		{"abcdef", 3, "abc"},
		{"ab\u00e9cd", 3, "ab"}, // é is 2 bytes at offsets 2-3: don't split it
		{"ab\u00e9cd", 4, "ab\u00e9"},
		{"a\xffb", 10, "a\uFFFDb"},
	}
	for _, c := range cases {
		got := truncateUTF8(c.in, c.n)
		if got != c.want || !utf8.ValidString(got) {
			t.Errorf("truncateUTF8(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
	}
}

func TestPeerPrefsBackupKeepsOriginal(t *testing.T) {
	s := newTestStore(t)
	orig := PeerPrefs{Callsign: "N0HQ-1", SendPath: "rf_only", WaitForAck: true}
	if saved, err := s.SavePeerPrefs(ctx, orig); err != nil || !saved {
		t.Fatalf("first save = %v, %v", saved, err)
	}
	// After a restart the app sees its own wait_for_ack=false; that must
	// not replace the true original.
	if saved, err := s.SavePeerPrefs(ctx, PeerPrefs{Callsign: "N0HQ-1", WaitForAck: false}); err != nil || saved {
		t.Fatalf("second save = %v, %v; want kept original", saved, err)
	}
	if _, err := s.SavePeerPrefs(ctx, PeerPrefs{Callsign: "bad call"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bad call err = %v", err)
	}
	got, err := s.ListPeerPrefs(ctx)
	if err != nil || len(got) != 1 || !got[0].WaitForAck || got[0].SendPath != "rf_only" {
		t.Fatalf("ListPeerPrefs = %+v, %v", got, err)
	}
	if err := s.DeletePeerPrefs(ctx, "N0HQ-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeletePeerPrefs(ctx, "N0HQ-1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete err = %v", err)
	}
}
