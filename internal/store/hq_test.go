package store

import (
	"checkin-board/internal/wire"
	"errors"
	"reflect"
	"testing"
	"time"
)

func report(t *testing.T, text string) *Report {
	t.Helper()
	m, err := wire.Decode(text)
	if err != nil {
		t.Fatalf("wire.Decode(%q): %v", text, err)
	}
	return m.(*Report)
}

func mustIngest(t *testing.T, s *Store, text string, receivedAt time.Time) IngestResult {
	t.Helper()
	res, err := s.IngestReport(ctx, report(t, text), "N0CALL-7", 0, receivedAt)
	if err != nil {
		t.Fatalf("IngestReport(%q): %v", text, err)
	}
	return res
}

func effective(t *testing.T, s *Store, f EntryFilter) []EffectiveEntry {
	t.Helper()
	got, err := s.EffectiveEntries(ctx, f)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestIngestReportStoresEntries(t *testing.T) {
	s := newTestStore(t)
	res := mustIngest(t, s, "RC1 R 3 1 @1300 101/05 104/22", at(time.Minute))
	if res.Duplicate || res.SeqReused || res.Entries != 2 {
		t.Fatalf("result = %+v", res)
	}
	got := effective(t, s, EntryFilter{})
	want := []EffectiveEntry{
		{CPCode: "3", Bib: 101, TimeIn: at(5 * time.Second), Count: 1, SourceCall: "N0CALL-7", ReceivedAt: at(time.Minute)},
		{CPCode: "3", Bib: 104, TimeIn: at(22 * time.Second), Count: 1, SourceCall: "N0CALL-7", ReceivedAt: at(time.Minute)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %+v\nwant: %+v", got, want)
	}
}

func TestIngestResolvesDateAcrossMidnight(t *testing.T) {
	s := newTestStore(t)
	recv := time.Date(2026, 10, 11, 0, 0, 30, 0, time.UTC)
	mustIngest(t, s, "RC1 R 3 1 @2359 7/50", recv)
	got := effective(t, s, EntryFilter{})
	if want := time.Date(2026, 10, 10, 23, 59, 50, 0, time.UTC); len(got) != 1 || !got[0].TimeIn.Equal(want) {
		t.Fatalf("TimeIn = %+v, want %v", got, want)
	}
}

func TestIngestDuplicateBatchIgnored(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", t0)
	res := mustIngest(t, s, "RC1 R 3 1 @1300 101/05", at(time.Minute))
	if !res.Duplicate || res.Entries != 0 {
		t.Fatalf("result = %+v, want duplicate", res)
	}
	if got := effective(t, s, EntryFilter{}); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("entries = %+v", got)
	}
}

// A checkpoint whose DB was wiped mid-race restarts at seq 1. Different
// text under a known seq is new data: keep it and flag the checkpoint.
func TestIngestSeqReuseKeepsNewData(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", t0)
	res := mustIngest(t, s, "RC1 R 3 1 @1310 200/00", at(10*time.Minute))
	if res.Duplicate || !res.SeqReused || res.Entries != 1 {
		t.Fatalf("result = %+v, want seq reuse with 1 entry", res)
	}
	// The reused batch is itself deduplicated on retransmit.
	if res := mustIngest(t, s, "RC1 R 3 1 @1310 200/00", at(11*time.Minute)); !res.Duplicate {
		t.Fatalf("retransmit of reused batch = %+v, want duplicate", res)
	}
	if got := effective(t, s, EntryFilter{}); len(got) != 2 {
		t.Fatalf("entries = %+v, want both", got)
	}
	st := statusFor(t, s, "3")
	if st.SeqReuseCount != 1 {
		t.Fatalf("SeqReuseCount = %d, want 1", st.SeqReuseCount)
	}
}

// The double-tap case: a volunteer logs 101 twice in the same second,
// then voids one copy. The runner must still show up once.
func TestDoubleTapThenVoidKeepsOneEntry(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05 101/05", t0)
	if got := effective(t, s, EntryFilter{}); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("after double tap = %+v, want one entry with Count 2", got)
	}
	mustIngest(t, s, "RC1 R 3 2 @1300 -101/05", at(time.Minute))
	if got := effective(t, s, EntryFilter{}); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("after void = %+v, want one entry with Count 1", got)
	}
}

func TestVoidRemovesEntry(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05 104/22", t0)
	mustIngest(t, s, "RC1 R 3 2 @1300 -101/05", at(time.Minute))
	got := effective(t, s, EntryFilter{})
	if len(got) != 1 || got[0].Bib != 104 {
		t.Fatalf("entries = %+v, want only 104", got)
	}
}

// Batches can arrive out of order (a gap resend). A void that lands
// before its original must still cancel it.
func TestVoidBeforeOriginalStillCancels(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 2 @1300 -101/05", t0)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", at(time.Minute))
	if got := effective(t, s, EntryFilter{}); len(got) != 0 {
		t.Fatalf("entries = %+v, want none", got)
	}
}

func TestEffectiveEntriesFilters(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05 104/22", t0)
	mustIngest(t, s, "RC1 R 4 1 @1400 101/10", at(time.Hour))
	bib := Bib(101)
	got := effective(t, s, EntryFilter{Bib: &bib})
	if len(got) != 2 || got[0].CPCode != "3" || got[1].CPCode != "4" {
		t.Fatalf("by bib = %+v, want cp 3 then 4 (time order)", got)
	}
	if got := effective(t, s, EntryFilter{CPCode: "4"}); len(got) != 1 || got[0].Bib != 101 {
		t.Fatalf("by cp = %+v", got)
	}
}

// Out-and-back: the same runner at the same checkpoint twice is two entries.
func TestOutAndBackKeepsBothPasses(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", t0)
	mustIngest(t, s, "RC1 R 3 2 @1500 101/40", at(2*time.Hour))
	if got := effective(t, s, EntryFilter{}); len(got) != 2 {
		t.Fatalf("entries = %+v, want both passes", got)
	}
}

func TestHQLocalEntries(t *testing.T) {
	s := newTestStore(t)
	e, err := s.LogHQLocal(ctx, "FIN", 101, at(5*time.Second+300*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if !e.Local() || e.BatchSeq != nil || !e.TimeIn.Equal(at(5*time.Second)) {
		t.Fatalf("entry = %+v", e)
	}
	got := effective(t, s, EntryFilter{})
	if len(got) != 1 || got[0].SourceCall != "" || got[0].CPCode != "FIN" {
		t.Fatalf("entries = %+v", got)
	}
	if err := s.VoidHQLocal(ctx, e.ID, at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := effective(t, s, EntryFilter{}); len(got) != 0 {
		t.Fatalf("entries after void = %+v", got)
	}
	if err := s.VoidHQLocal(ctx, e.ID, at(time.Minute)); !errors.Is(err, ErrAlreadyVoided) {
		t.Fatalf("double void err = %v", err)
	}
	if _, err := s.LogHQLocal(ctx, "fin", 1, t0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad cp err = %v", err)
	}
	if _, err := s.LogHQLocal(ctx, "FIN", 10000, t0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad bib err = %v", err)
	}
}

func TestVoidHQLocalRejectsRemoteAndMissing(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", t0)
	var remote ReceivedEntry
	if err := s.db.First(&remote).Error; err != nil {
		t.Fatal(err)
	}
	if err := s.VoidHQLocal(ctx, remote.ID, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("void remote err = %v, want ErrNotFound", err)
	}
	if err := s.VoidHQLocal(ctx, 999, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("void missing err = %v, want ErrNotFound", err)
	}
}

func statusFor(t *testing.T, s *Store, cp string) CheckpointStatus {
	t.Helper()
	all, err := s.ListStatuses(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range all {
		if st.CPCode == cp {
			return st
		}
	}
	t.Fatalf("no status for %s in %+v", cp, all)
	return CheckpointStatus{}
}

func TestStatusTracksIngestAndHeartbeat(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 101/05", at(time.Minute))
	st := statusFor(t, s, "3")
	if st.MaxSeq != 1 || st.BatchesReceived != 1 || st.LastSourceCall != "N0CALL-7" ||
		st.LastHeardAt == nil || !st.LastHeardAt.Equal(at(time.Minute)) || st.HeartbeatAt != nil {
		t.Fatalf("status after ingest = %+v", st)
	}

	// Checkpoint clock reads 13:04:30 when HQ's race clock reads 13:05:00: 30 s slow.
	hb := &Heartbeat{CP: "3", LastSeq: 4, Time: wire.TimeOfDayOf(at(4*time.Minute + 30*time.Second))}
	if err := s.RecordHeartbeat(ctx, hb, "N0CALL-7", at(5*time.Minute), at(5*time.Minute), true); err != nil {
		t.Fatal(err)
	}
	st = statusFor(t, s, "3")
	if st.HeartbeatLastSeq != 4 || st.HeartbeatAt == nil || st.ClockSkewSec == nil || *st.ClockSkewSec != -30 ||
		!st.LastHeardAt.Equal(at(5*time.Minute)) {
		t.Fatalf("status after heartbeat = %+v", st)
	}
	// A heartbeat never lowers MaxSeq.
	if st.MaxSeq != 1 {
		t.Fatalf("MaxSeq = %d, want 1", st.MaxSeq)
	}
}

func TestMissingSeqs(t *testing.T) {
	s := newTestStore(t)
	mustIngest(t, s, "RC1 R 3 1 @1300 1/00", t0)
	mustIngest(t, s, "RC1 R 3 3 @1300 3/00", t0)
	mustIngest(t, s, "RC1 R 3 6 @1300 6/00", t0)
	got, err := s.MissingSeqs(ctx, "3", wire.MaxGapSeqs)
	if err != nil || !reflect.DeepEqual(got, []uint32{2, 4, 5}) {
		t.Fatalf("MissingSeqs = %v, %v; want [2 4 5]", got, err)
	}

	// The heartbeat reveals a lost tail: batches 7 and 8.
	hb := &Heartbeat{CP: "3", LastSeq: 8, Time: wire.TimeOfDayOf(t0)}
	if err := s.RecordHeartbeat(ctx, hb, "N0CALL-7", t0, t0, true); err != nil {
		t.Fatal(err)
	}
	got, _ = s.MissingSeqs(ctx, "3", wire.MaxGapSeqs)
	if !reflect.DeepEqual(got, []uint32{2, 4, 5, 7, 8}) {
		t.Fatalf("MissingSeqs with tail = %v", got)
	}
	if got, _ := s.MissingSeqs(ctx, "3", 2); !reflect.DeepEqual(got, []uint32{2, 4}) {
		t.Fatalf("limited = %v", got)
	}
	if got, _ := s.MissingSeqs(ctx, "NONE", wire.MaxGapSeqs); len(got) != 0 {
		t.Fatalf("unknown cp = %v", got)
	}
}

// A bogus heartbeat claiming lastseq 4e9 must not make HQ loop for ages.
func TestMissingSeqsBoundedByLimit(t *testing.T) {
	s := newTestStore(t)
	// RecordHeartbeat refuses implausible lastseqs, so write one directly:
	// MissingSeqs itself must still stay bounded.
	hb := &Heartbeat{CP: "3", LastSeq: 1, Time: wire.TimeOfDayOf(t0)}
	if err := s.RecordHeartbeat(ctx, hb, "N0CALL-7", t0, t0, true); err != nil {
		t.Fatal(err)
	}
	if err := s.db.Exec("UPDATE cp_status SET heartbeat_last_seq = 4000000000").Error; err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	got, err := s.MissingSeqs(ctx, "3", wire.MaxGapSeqs)
	if err != nil || len(got) != wire.MaxGapSeqs || got[0] != 1 {
		t.Fatalf("len = %d, err = %v", len(got), err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("MissingSeqs took %v", time.Since(start))
	}
}

// A closed checkpoint's heartbeats (4.7) stamp closed_at once; a later
// heartbeat without the flag (a new race after a reset) clears it.
func TestHeartbeatClosedFlagMarksCheckpointClosed(t *testing.T) {
	s := newTestStore(t)
	hb := &Heartbeat{CP: "AS5", LastSeq: 3, Time: wire.TimeOfDayOf(t0)}
	if err := s.RecordHeartbeat(ctx, hb, "N0CALL-7", t0, t0, true); err != nil {
		t.Fatal(err)
	}
	closed := *hb
	closed.Closed = true
	for i := range 2 {
		if err := s.RecordHeartbeat(ctx, &closed, "N0CALL-7", at(time.Duration(i+1)*time.Minute), t0, true); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.ListStatuses(ctx)
	if len(st) != 1 || st[0].ClosedAt == nil || !st[0].ClosedAt.Equal(at(time.Minute)) {
		t.Fatalf("status = %+v", st)
	}
	if err := s.RecordHeartbeat(ctx, hb, "N0CALL-7", at(time.Hour), t0, true); err != nil {
		t.Fatal(err)
	}
	if st, _ = s.ListStatuses(ctx); st[0].ClosedAt != nil {
		t.Fatalf("reopened checkpoint still closed: %+v", st[0])
	}
}
