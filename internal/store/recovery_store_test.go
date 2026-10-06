package store

import (
	"errors"
	"testing"
	"time"

	"checkin-board/internal/wire"
)

func decodeReport(t *testing.T, text string) *Report {
	t.Helper()
	msg, err := wire.Decode(text)
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	return msg.(*Report)
}

func TestIngestImportedUsesExactTimesAndDedupsWithRadio(t *testing.T) {
	s := newTestStore(t)
	rep := decodeReport(t, "RC1 R AS5 1 @1300 101/05 102/30")
	// Exact times from the export; the second is a day earlier than the
	// time-of-day would resolve to, which only the export can express.
	times := []time.Time{at(5 * time.Second), at(30*time.Second - 24*time.Hour)}

	res, err := s.IngestImported(ctx, rep, times, at(time.Hour))
	if err != nil || res.Duplicate || res.Entries != 2 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	got, _ := s.EffectiveEntries(ctx, EntryFilter{})
	if len(got) != 2 || !got[0].TimeIn.Equal(times[1]) || got[0].SourceCall != SourceImport {
		t.Fatalf("effective = %+v", got)
	}
	// Import doesn't count as hearing the checkpoint.
	sts, _ := s.ListStatuses(ctx)
	if len(sts) != 1 || sts[0].LastHeardAt != nil || sts[0].BatchesReceived != 1 {
		t.Fatalf("status = %+v", sts)
	}
	// The same batch later arrives by radio: a duplicate, not new runners.
	if r, err := s.IngestReport(ctx, rep, "N0CALL-1", 0, at(2*time.Hour)); err != nil || !r.Duplicate {
		t.Fatalf("radio copy = %+v, %v", r, err)
	}
	if _, err := s.IngestImported(ctx, rep, times[:1], t0); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("mismatched times err = %v", err)
	}
}

func TestIngestImportedBatchesIsAllOrNothing(t *testing.T) {
	s := newTestStore(t)
	good := ImportedBatch{Report: decodeReport(t, "RC1 R AS5 1 @1300 101/05"), Times: []time.Time{at(5 * time.Second)}}
	bad := ImportedBatch{Report: decodeReport(t, "RC1 R AS5 2 @1301 102/05"), Times: nil}

	if _, err := s.IngestImportedBatches(ctx, []ImportedBatch{good, bad}, t0); err == nil {
		t.Fatal("expected error")
	}
	if got, _ := s.EffectiveEntries(ctx, EntryFilter{}); len(got) != 0 {
		t.Fatalf("partial import stored %+v", got)
	}
	res, err := s.IngestImportedBatches(ctx, []ImportedBatch{good}, t0)
	if err != nil || len(res) != 1 || res[0].Entries != 1 {
		t.Fatalf("good import = %+v, %v", res, err)
	}
}

func TestReconcilePassages(t *testing.T) {
	s := newTestStore(t)
	k1 := PassageKey{CP: "AS5", Bib: 101, Unix: at(time.Minute).Unix()}
	k2 := PassageKey{CP: "AS5", Bib: 102, Unix: at(2 * time.Minute).Unix()}
	k3 := PassageKey{CP: "AS5", Bib: 103, Unix: at(3 * time.Minute).Unix()}
	// HQ has k2 twice and a lone void for k3 (its entry's batch is missing).
	events := []ReceivedEntry{
		{CPCode: "AS5", Bib: 102, TimeIn: time.Unix(k2.Unix, 0), ReceivedAt: t0},
		{CPCode: "AS5", Bib: 102, TimeIn: time.Unix(k2.Unix, 0), ReceivedAt: t0},
		{CPCode: "AS5", Bib: 103, TimeIn: time.Unix(k3.Unix, 0), IsVoid: true, ReceivedAt: t0},
	}
	if err := s.AppendEvents(ctx, events); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendEvents(ctx, nil); err != nil {
		t.Fatal(err)
	}
	have, err := s.NetCounts(ctx, []string{"AS5"})
	if err != nil || have[k2] != 2 || have[k3] != -1 {
		t.Fatalf("NetCounts = %v, %v", have, err)
	}

	// The journal says: k1 once, k2 once, k3 once.
	res, err := s.ReconcilePassages(ctx, map[PassageKey]int{k1: 1, k2: 1, k3: 1}, SourceJournal, at(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res != (ReconcileResult{Added: 1, Voided: 1, Deferred: 1}) {
		t.Fatalf("result = %+v", res)
	}
	after, _ := s.NetCounts(ctx, nil)
	if after[k1] != 1 || after[k2] != 1 || after[k3] != -1 {
		t.Fatalf("after = %v", after)
	}
	// Reconciling again changes nothing.
	again, _ := s.ReconcilePassages(ctx, map[PassageKey]int{k1: 1, k2: 1}, SourceJournal, at(time.Hour))
	if again != (ReconcileResult{}) {
		t.Fatalf("second reconcile = %+v", again)
	}
}

func TestGetReceivedEntry(t *testing.T) {
	s := newTestStore(t)
	e, err := s.LogHQLocal(ctx, "FIN", 7, at(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetReceivedEntry(ctx, e.ID)
	if err != nil || got.Bib != 7 || !got.TimeIn.Equal(at(time.Second)) || !got.Local() {
		t.Fatalf("GetReceivedEntry = %+v, %v", got, err)
	}
	if _, err := s.GetReceivedEntry(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing err = %v", err)
	}
}

func TestLocalEntryLookupsAndExport(t *testing.T) {
	s := newTestStore(t)
	e1 := mustLog(t, s, "AS5", 101, at(5*time.Second))
	mustLog(t, s, "AS5", 102, at(6*time.Second))
	b, _ := sendBatch(t, s, "AS5", t0)
	if _, err := s.VoidLocal(ctx, e1.ID); err != nil {
		t.Fatal(err)
	}
	mustLog(t, s, "OTHER", 9, at(7*time.Second))

	got, err := s.GetLocalEntry(ctx, e1.ID)
	if err != nil || got.Bib != 101 {
		t.Fatalf("GetLocalEntry = %+v, %v", got, err)
	}
	if _, err := s.GetLocalEntry(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing err = %v", err)
	}
	codes, err := s.QueuedCodes(ctx)
	if err != nil || len(codes) != 2 || codes[0] != "AS5" || codes[1] != "OTHER" {
		t.Fatalf("QueuedCodes = %v, %v; want the void's code and OTHER", codes, err)
	}

	rows, err := s.ExportRows(ctx)
	if err != nil || len(rows) != 4 {
		t.Fatalf("ExportRows = %+v, %v", rows, err)
	}
	if rows[0].Seq != b.Seq || rows[0].Bib != 101 || rows[0].Void {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if !rows[2].Void || rows[2].Seq != 0 || rows[2].Bib != 101 {
		t.Errorf("void row = %+v, want unbatched void of 101", rows[2])
	}
}
