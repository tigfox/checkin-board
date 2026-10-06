package ops

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"checkin-board/internal/journal"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// exportHeader is the CSV header of a checkpoint export. msg_id is the
// graywolf msgid of the batch's row, for cross-checking against
// graywolf's packet log; imports also accept files without it.
const exportHeader = "cp,seq,event,bib,time_in,clock_synced,msg_id"

// Import caps. A real race is far below these (500 runners × ~10
// checkpoints); they stop a wrong or hostile file from tying up the HQ
// database. The upload itself is also size-capped by the HTTP layer.
const (
	maxImportRows        = 50_000  // export rows
	maxImportBatches     = 5_000   // distinct (cp, seq) in one export
	maxJournalLines      = 200_000 // journal records
	maxImportCheckpoints = 100     // distinct checkpoint codes per file
	maxReportedRowErrors = 20
)

// ExportCheckpoint writes this checkpoint's full event log (entries and
// voids, with batch seqs) as CSV for manual delivery to HQ: the offline
// path for a final check-in when RF can't finish it. Everything still
// queued is batched first, so every row has a seq; those batches also
// go out by radio as usual, and HQ dedups the two paths. Returns the
// number of rows written.
func (s *Service) ExportCheckpoint(ctx context.Context, w io.Writer) (int, error) {
	cfg, err := s.settings(ctx)
	if err != nil {
		return 0, err
	}
	if cfg.Role != store.RoleCheckpoint {
		return 0, ErrWrongRole
	}
	codes, err := s.cfg.Store.QueuedCodes(ctx)
	if err != nil {
		return 0, err
	}
	now := s.now()
	for _, code := range codes {
		for {
			b, err := s.cfg.Store.CreateBatch(ctx, code, cfg.MaxTextLen, now)
			if err != nil {
				return 0, err
			}
			if b == nil {
				break
			}
		}
	}
	rows, err := s.cfg.Store.ExportRows(ctx)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.Seq == 0 {
			return 0, fmt.Errorf("ops: export: %s bib %d has no batch", r.CP, r.Bib)
		}
	}
	cw := csv.NewWriter(w)
	_ = cw.Write(strings.Split(exportHeader, ","))
	for _, r := range rows {
		event := journal.EventEntry
		if r.Void {
			event = journal.EventVoid
		}
		_ = cw.Write([]string{r.CP, strconv.FormatUint(uint64(r.Seq), 10), event,
			strconv.Itoa(int(r.Bib)), r.TimeIn.Format(time.RFC3339), strconv.FormatBool(r.ClockSynced), r.GWMsgID})
	}
	cw.Flush()
	return len(rows), cw.Error()
}

// ImportResult summarizes an import at HQ.
type ImportResult struct {
	Batches    int // batches in the file
	Duplicates int // already held (received by radio or imported before)
	Entries    int // entry/void events stored
}

// ImportCheckpointExport ingests a checkpoint export at HQ. Each batch
// is rebuilt from its rows and stored through the same (cp, seq, text)
// dedup as radio traffic; the rebuilt text is byte-identical to the
// on-air batch, so importing is idempotent and safe alongside radio
// delivery. The whole file is validated before anything is stored.
func (s *Service) ImportCheckpointExport(ctx context.Context, r io.Reader) (ImportResult, error) {
	var res ImportResult
	if err := s.requireHQ(ctx); err != nil {
		return res, err
	}
	groups, err := parseExport(r)
	if err != nil {
		return res, err
	}
	batches := make([]store.ImportedBatch, len(groups))
	for i, g := range groups {
		batches[i] = store.ImportedBatch{Report: g.report, Times: g.times}
	}
	now, _ := s.cfg.Clock.Now()
	// One transaction, not cancelled by a client disconnect: the file is
	// applied whole or not at all.
	results, err := s.cfg.Store.IngestImportedBatches(context.WithoutCancel(ctx), batches, now)
	if err != nil {
		return res, fmt.Errorf("ops: import: %w", err)
	}
	for _, ir := range results {
		res.Batches++
		res.Entries += ir.Entries
		if ir.Duplicate {
			res.Duplicates++
		}
	}
	return res, nil
}

func (s *Service) requireHQ(ctx context.Context) error {
	cfg, err := s.settings(ctx)
	if err != nil {
		return err
	}
	if cfg.Role != store.RoleHQ {
		return ErrWrongRole
	}
	return nil
}

type exportGroup struct {
	report *wire.Report
	times  []time.Time // exact time of each entry, parallel to report.Entries
}

// parseExport groups export rows into batches, preserving row order.
func parseExport(r io.Reader) ([]exportGroup, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	var (
		groups []exportGroup
		index  = map[[2]string]int{}
		bad    rowErrors
		line   int
		cps    = map[string]bool{}
	)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		line++
		if line > maxImportRows+1 { // +1 for the header
			return nil, &ImportError{What: "checkpoint export", Reason: fmt.Sprintf("more than %d rows", maxImportRows)}
		}
		if err != nil {
			bad.add(line, err.Error())
			continue
		}
		if line == 1 && strings.HasPrefix(strings.Join(rec, ","), "cp,seq,event") {
			continue
		}
		row, msg := parseExportRow(rec)
		if msg != "" {
			bad.add(line, msg)
			continue
		}
		if cps[row.CP] = true; len(cps) > maxImportCheckpoints {
			return nil, &ImportError{What: "checkpoint export", Reason: fmt.Sprintf("more than %d checkpoint codes", maxImportCheckpoints)}
		}
		k := [2]string{row.CP, strconv.FormatUint(uint64(row.Seq), 10)}
		i, ok := index[k]
		if !ok {
			if len(groups) == maxImportBatches {
				return nil, &ImportError{What: "checkpoint export", Reason: fmt.Sprintf("more than %d batches", maxImportBatches)}
			}
			i = len(groups)
			index[k] = i
			groups = append(groups, exportGroup{report: &wire.Report{CP: row.CP, Seq: row.Seq}})
		}
		groups[i].report.Entries = append(groups[i].report.Entries,
			wire.Entry{Bib: row.Bib, Time: wire.TimeOfDayOf(row.TimeIn), Void: row.Void})
		groups[i].times = append(groups[i].times, row.TimeIn)
	}
	if !bad.empty() || len(groups) == 0 {
		return nil, &ImportError{What: "checkpoint export", Rows: bad.Rows, More: bad.More}
	}
	return groups, nil
}

func parseExportRow(f []string) (store.ExportRow, string) {
	if len(f) != 6 && len(f) != 7 {
		return store.ExportRow{}, "expected " + exportHeader
	}
	if !wire.ValidCheckpointCode(f[0]) {
		return store.ExportRow{}, fmt.Sprintf("checkpoint code %q", f[0])
	}
	seq, err := strconv.ParseUint(f[1], 10, 32)
	if err != nil || seq == 0 {
		return store.ExportRow{}, fmt.Sprintf("seq %q must be a batch number >= 1", f[1])
	}
	if f[2] != journal.EventEntry && f[2] != journal.EventVoid {
		return store.ExportRow{}, fmt.Sprintf("event %q must be entry or void", f[2])
	}
	bib, err := wire.ParseBib(f[3])
	if err != nil {
		return store.ExportRow{}, fmt.Sprintf("bib %q must be 1-9999", f[3])
	}
	t, err := time.Parse(time.RFC3339, f[4])
	if err != nil {
		return store.ExportRow{}, fmt.Sprintf("time %q must be RFC 3339", f[4])
	}
	synced, err := strconv.ParseBool(f[5])
	if err != nil {
		return store.ExportRow{}, fmt.Sprintf("clock_synced %q must be true or false", f[5])
	}
	return store.ExportRow{CP: f[0], Seq: uint32(seq), Void: f[2] == journal.EventVoid, Bib: bib,
		TimeIn: t.UTC(), ClockSynced: synced}, ""
}

// RowError is one rejected import line.
type RowError struct {
	Line int
	Msg  string
}

// rowErrors collects bad lines up to maxReportedRowErrors, counting the
// rest, so a wrong file can't build a huge error message.
type rowErrors struct {
	Rows []RowError
	More int
}

func (r *rowErrors) add(line int, msg string) {
	if len(r.Rows) < maxReportedRowErrors {
		r.Rows = append(r.Rows, RowError{Line: line, Msg: msg})
		return
	}
	r.More++
}

func (r *rowErrors) empty() bool { return len(r.Rows) == 0 && r.More == 0 }

// ImportError lists the bad lines in an import file (the first
// maxReportedRowErrors), or rejects it as a whole. Wraps
// store.ErrInvalidInput.
type ImportError struct {
	What   string
	Rows   []RowError
	More   int    // bad lines beyond Rows
	Reason string // set when the whole file is rejected (e.g. a cap)
}

func (e *ImportError) Error() string {
	switch {
	case e.Reason != "":
		return "ops: invalid " + e.What + ": " + e.Reason
	case len(e.Rows) == 0:
		return "ops: " + e.What + " has no rows"
	}
	parts := make([]string, len(e.Rows))
	for i, row := range e.Rows {
		parts[i] = fmt.Sprintf("line %d: %s", row.Line, row.Msg)
	}
	msg := "ops: invalid " + e.What + ": " + strings.Join(parts, "; ")
	if e.More > 0 {
		msg += fmt.Sprintf("; and %d more", e.More)
	}
	return msg
}

func (e *ImportError) Unwrap() error { return store.ErrInvalidInput }

// JournalImportResult summarizes a journal import at HQ.
type JournalImportResult struct {
	Added   int // entry events added
	Voided  int // void events added
	Skipped int // unreadable journal lines
	// Deferred counts passages left alone because HQ holds a void whose
	// entry batch is still missing; re-import once it arrives.
	Deferred int
	// MissingBatches lists, per checkpoint in the journal, batches HQ is
	// still missing. If they later arrive carrying passages this import
	// added, the board shows Count 2. Prefer an export import when the
	// checkpoint's database survives.
	MissingBatches map[string][]uint32
}

// ImportJournal reconciles HQ to a bib journal, for when a checkpoint's
// database is lost. The journal has no batch seqs, so for every passage
// it mentions, HQ's net count is brought to the journal's net count
// with entry or void events (source JOURNAL). Idempotent.
func (s *Service) ImportJournal(ctx context.Context, r io.Reader) (JournalImportResult, error) {
	var res JournalImportResult
	if err := s.requireHQ(ctx); err != nil {
		return res, err
	}
	recs, skipped, err := journal.Parse(r)
	res.Skipped = skipped
	if err != nil {
		return res, err
	}
	if len(recs) == 0 {
		return res, &ImportError{What: "journal"}
	}
	if len(recs) > maxJournalLines {
		return res, &ImportError{What: "journal", Reason: fmt.Sprintf("more than %d records", maxJournalLines)}
	}
	want := map[store.PassageKey]int{}
	cpSet := map[string]bool{}
	for _, rec := range recs {
		k := store.PassageKey{CP: rec.CP, Bib: rec.Bib, Unix: rec.TimeIn.UTC().Truncate(time.Second).Unix()}
		if rec.Event == journal.EventVoid {
			want[k]--
		} else {
			want[k]++
		}
		cpSet[rec.CP] = true
	}
	if len(cpSet) > maxImportCheckpoints {
		return res, &ImportError{What: "journal", Reason: fmt.Sprintf("more than %d checkpoint codes", maxImportCheckpoints)}
	}
	now, _ := s.cfg.Clock.Now()
	rr, err := s.cfg.Store.ReconcilePassages(ctx, want, store.SourceJournal, now)
	if err != nil {
		return res, err
	}
	res.Added, res.Voided, res.Deferred = rr.Added, rr.Voided, rr.Deferred
	cps := make([]string, 0, len(cpSet))
	for cp := range cpSet {
		cps = append(cps, cp)
	}
	slices.Sort(cps)
	for _, cp := range cps {
		missing, err := s.cfg.Store.MissingSeqs(ctx, cp, wire.MaxGapSeqs)
		if err != nil {
			return res, err
		}
		if len(missing) > 0 {
			if res.MissingBatches == nil {
				res.MissingBatches = map[string][]uint32{}
			}
			res.MissingBatches[cp] = missing
		}
	}
	return res, nil
}
