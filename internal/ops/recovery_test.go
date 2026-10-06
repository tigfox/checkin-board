package ops

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/journal"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// exportFrom logs bibs at a checkpoint and returns its export.
func exportFrom(t *testing.T, bibs ...store.Bib) (*env, string) {
	t.Helper()
	cp := newEnv(t, checkpointSettings(), store.RaceActive)
	for _, b := range bibs {
		cp.logBib("AS5", b)
		cp.ft.Advance(3 * time.Second)
	}
	var buf bytes.Buffer
	n, err := cp.svc.ExportCheckpoint(ctx, &buf)
	if err != nil || n != len(bibs) {
		t.Fatalf("export = %d rows, %v", n, err)
	}
	return cp, buf.String()
}

func TestExportImportIsExactAndIdempotent(t *testing.T) {
	cp, csv := exportFrom(t, 101, 102, 103)
	if !strings.HasPrefix(csv, exportHeader+"\n") {
		t.Fatalf("export header: %q", csv)
	}
	hqe := newEnv(t, hqSettings(), store.RaceComplete)
	res, err := hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(csv))
	if err != nil || res.Entries != 3 || res.Duplicates != 0 {
		t.Fatalf("import = %+v, %v", res, err)
	}
	// Importing again (or the same batches arriving by radio) adds nothing.
	res, err = hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(csv))
	if err != nil || res.Duplicates != res.Batches || res.Entries != 0 {
		t.Fatalf("re-import = %+v, %v", res, err)
	}
	// The rebuilt batch text is byte-identical to what went on air.
	pending, _ := cp.st.ListPendingBatches(ctx)
	msg, _ := wire.Decode(pending[0].Text)
	if r, _ := hqe.st.IngestReport(ctx, msg.(*wire.Report), "K1CP", 0, t0); !r.Duplicate {
		t.Fatal("radio copy of an imported batch was not a duplicate")
	}
	got, _ := hqe.st.EffectiveEntries(ctx, store.EntryFilter{})
	if len(got) != 3 || !got[1].TimeIn.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("entries = %+v", got)
	}
}

func TestImportAcceptsLegacySixColumnExports(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	legacy := "cp,seq,event,bib,time_in,clock_synced\nAS5,1,entry,7,2026-10-10T13:00:05Z,true\n"
	if res, err := hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(legacy)); err != nil || res.Entries != 1 {
		t.Fatalf("import = %+v, %v", res, err)
	}
}

func TestImportRejectsBadFiles(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	bad := []string{
		"",
		exportHeader + "\n",
		exportHeader + "\nas5,1,entry,7,2026-10-10T13:00:05Z,true,\n",
		exportHeader + "\nAS5,0,entry,7,2026-10-10T13:00:05Z,true,\n",
		exportHeader + "\nAS5,1,typo,7,2026-10-10T13:00:05Z,true,\n",
		exportHeader + "\nAS5,1,entry,0,2026-10-10T13:00:05Z,true,\n",
		exportHeader + "\nAS5,1,entry,7,yesterday,true,\n",
		exportHeader + "\nAS5,1,entry,7,2026-10-10T13:00:05Z,maybe,\n",
		"a,b\n",
	}
	for _, f := range bad {
		_, err := hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(f))
		var ie *ImportError
		if !errors.As(err, &ie) || !errors.Is(err, store.ErrInvalidInput) {
			t.Errorf("import of %q: err = %v, want *ImportError", f, err)
		}
	}
	if got, _ := hqe.st.EffectiveEntries(ctx, store.EntryFilter{}); len(got) != 0 {
		t.Fatalf("bad files stored entries: %+v", got)
	}
}

func TestImportErrorListsBoundedLines(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	var b strings.Builder
	b.WriteString(exportHeader + "\n")
	for range 30 {
		b.WriteString("AS5,1,entry,0,2026-10-10T13:00:05Z,true,\n")
	}
	_, err := hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(b.String()))
	var ie *ImportError
	if !errors.As(err, &ie) || len(ie.Rows) != maxReportedRowErrors || ie.More != 10 || !strings.Contains(err.Error(), "and 10 more") {
		t.Fatalf("err = %v", err)
	}
}

func TestImportCaps(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	var b strings.Builder
	b.WriteString(exportHeader + "\n")
	for i := range maxImportCheckpoints + 1 {
		fmt.Fprintf(&b, "C%d,1,entry,7,2026-10-10T13:00:05Z,true,\n", i)
	}
	if _, err := hqe.svc.ImportCheckpointExport(ctx, strings.NewReader(b.String())); err == nil ||
		!strings.Contains(err.Error(), "checkpoint codes") {
		t.Fatalf("err = %v", err)
	}
}

func TestImportsAreHQOnlyAndExportIsCheckpointOnly(t *testing.T) {
	cp := newEnv(t, checkpointSettings(), store.RaceActive)
	if _, err := cp.svc.ImportCheckpointExport(ctx, strings.NewReader("x")); !errors.Is(err, ErrWrongRole) {
		t.Errorf("checkpoint import err = %v", err)
	}
	if _, err := cp.svc.ImportJournal(ctx, strings.NewReader("x")); !errors.Is(err, ErrWrongRole) {
		t.Errorf("checkpoint journal import err = %v", err)
	}
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	if _, err := hqe.svc.ExportCheckpoint(ctx, &bytes.Buffer{}); !errors.Is(err, ErrWrongRole) {
		t.Errorf("HQ export err = %v", err)
	}
}

func TestImportJournalReconciles(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	// HQ already has bib 1 by radio (batch 2; batch 1 is missing).
	msg, _ := wire.Decode("RC1 R AS5 2 @1300 1/00")
	if _, err := hqe.st.IngestReport(ctx, msg.(*wire.Report), "K1CP", 0, t0); err != nil {
		t.Fatal(err)
	}
	rec := func(event string, bib store.Bib, sec int) string {
		r := journal.Record{LoggedAt: t0, Event: event, CP: "AS5", Bib: bib, TimeIn: t0.Add(time.Duration(sec) * time.Second), ClockSynced: true}
		return fmt.Sprintf("%s,%s,%s,%d,%s,%t\n", r.LoggedAt.Format(time.RFC3339), r.Event, r.CP, r.Bib, r.TimeIn.Format(time.RFC3339), r.ClockSynced)
	}
	j := "logged_at,event,cp,bib,time_in,clock_synced\n" +
		rec(journal.EventEntry, 1, 0) + // already at HQ
		rec(journal.EventEntry, 2, 5) + // new
		rec(journal.EventEntry, 3, 9) + rec(journal.EventVoid, 3, 9) + // typo, voided
		"torn line,entr\n"
	res, err := hqe.svc.ImportJournal(ctx, strings.NewReader(j))
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 1 || res.Voided != 0 || res.Skipped != 1 || len(res.MissingBatches["AS5"]) != 1 {
		t.Fatalf("result = %+v", res)
	}
	got, _ := hqe.st.EffectiveEntries(ctx, store.EntryFilter{})
	if len(got) != 2 {
		t.Fatalf("entries = %+v", got)
	}
	if again, _ := hqe.svc.ImportJournal(ctx, strings.NewReader(j)); again.Added != 0 {
		t.Fatalf("re-import added %d", again.Added)
	}
	if _, err := hqe.svc.ImportJournal(ctx, strings.NewReader("")); err == nil {
		t.Fatal("empty journal accepted")
	}
}
