package ops

import (
	"cmp"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"slices"
	"strconv"
	"time"

	"checkin-board/internal/hq"
	"checkin-board/internal/store"
)

// sourceLabelHQ labels HQ keypad entries in exports (their SourceCall is "").
const sourceLabelHQ = "HQ"

// BoardCheckpoint is one board column.
type BoardCheckpoint struct {
	Code        string
	Name        string
	CourseOrder int
	// Defined is false for a code seen in reports but not configured as a
	// checkpoint at HQ (typo, or a station using the wrong code).
	Defined bool
}

// BoardCell is one runner at one checkpoint: every passage time (an
// out-and-back course passes twice), oldest first.
type BoardCell struct {
	Times []time.Time
	// Doubled is set when one passage was logged more than once and not
	// voided (a double tap, or a journal import overlapping radio data).
	Doubled bool
}

// BoardRunner is one board row, identified by bib (no personal data is
// ever stored). Cells align with Board.Checkpoints.
type BoardRunner struct {
	Bib        store.Bib
	Category   string
	InRoster   bool
	Cells      []BoardCell
	LastSeenCP string
	LastSeenAt *time.Time
}

// Board is HQ's race grid plus checkpoint health, so missing batches
// are visible right beside the times they may be hiding.
type Board struct {
	Checkpoints []BoardCheckpoint
	Runners     []BoardRunner
	Health      []hq.CheckpointHealth
	GeneratedAt time.Time
}

// Board builds the race grid: roster runners (seen or not) plus any
// unknown bibs that were reported, by bib; columns are configured
// checkpoints in course order, then undefined codes seen in reports.
func (s *Service) Board(ctx context.Context) (*Board, error) {
	if err := s.requireHQ(ctx); err != nil {
		return nil, err
	}
	cps, err := s.cfg.Store.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	runners, err := s.cfg.Store.ListRunners(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := s.cfg.Store.EffectiveEntries(ctx, store.EntryFilter{})
	if err != nil {
		return nil, err
	}
	health, err := s.cfg.HQ.Health(ctx)
	if err != nil {
		return nil, err
	}
	b := &Board{Checkpoints: boardColumns(cps, entries), Health: health, GeneratedAt: s.now().UTC()}
	col := make(map[string]int, len(b.Checkpoints))
	for i, c := range b.Checkpoints {
		col[c.Code] = i
	}
	rows := map[store.Bib]*BoardRunner{}
	row := func(bib store.Bib) *BoardRunner {
		if r := rows[bib]; r != nil {
			return r
		}
		r := &BoardRunner{Bib: bib, Cells: make([]BoardCell, len(b.Checkpoints))}
		rows[bib] = r
		return r
	}
	for _, rn := range runners {
		r := row(rn.Bib)
		r.Category, r.InRoster = rn.Category, true
	}
	for _, e := range entries { // time order
		r := row(e.Bib)
		cell := &r.Cells[col[e.CPCode]]
		cell.Times = append(cell.Times, e.TimeIn)
		cell.Doubled = cell.Doubled || e.Count > 1
		if r.LastSeenAt == nil || !e.TimeIn.Before(*r.LastSeenAt) {
			t := e.TimeIn
			r.LastSeenAt, r.LastSeenCP = &t, e.CPCode
		}
	}
	for _, r := range rows {
		b.Runners = append(b.Runners, *r)
	}
	slices.SortFunc(b.Runners, func(a, c BoardRunner) int { return cmp.Compare(a.Bib, c.Bib) })
	return b, nil
}

func boardColumns(cps []store.Checkpoint, entries []store.EffectiveEntry) []BoardCheckpoint {
	cols := make([]BoardCheckpoint, 0, len(cps))
	known := map[string]bool{}
	for _, c := range cps {
		cols = append(cols, BoardCheckpoint{Code: c.Code, Name: c.Name, CourseOrder: c.CourseOrder, Defined: true})
		known[c.Code] = true
	}
	var extra []string
	for _, e := range entries {
		if !known[e.CPCode] {
			known[e.CPCode] = true
			extra = append(extra, e.CPCode)
		}
	}
	slices.Sort(extra)
	for _, code := range extra {
		cols = append(cols, BoardCheckpoint{Code: code, Name: code})
	}
	return cols
}

// RunnerHistory is every passage of one bib, oldest first.
type RunnerHistory struct {
	Bib      store.Bib
	Category string
	InRoster bool
	Passages []store.EffectiveEntry
}

// RunnerHistory looks up one bib's passages (HQ only).
func (s *Service) RunnerHistory(ctx context.Context, bib store.Bib) (*RunnerHistory, error) {
	if err := s.requireHQ(ctx); err != nil {
		return nil, err
	}
	h := &RunnerHistory{Bib: bib}
	rn, err := s.cfg.Store.GetRunner(ctx, bib)
	switch {
	case err == nil:
		h.Category, h.InRoster = rn.Category, true
	case !errors.Is(err, store.ErrNotFound):
		return nil, err
	}
	if h.Passages, err = s.cfg.Store.EffectiveEntries(ctx, store.EntryFilter{Bib: &bib}); err != nil {
		return nil, err
	}
	return h, nil
}

// ExportResults writes every effective passage as CSV, in time order,
// for the timing crew (HQ only). Bib and category only: the crew joins
// names in their own software. Unbranded by design (spec 8.3).
func (s *Service) ExportResults(ctx context.Context, w io.Writer) error {
	if err := s.requireHQ(ctx); err != nil {
		return err
	}
	entries, err := s.cfg.Store.EffectiveEntries(ctx, store.EntryFilter{})
	if err != nil {
		return err
	}
	runners, err := s.cfg.Store.ListRunners(ctx)
	if err != nil {
		return err
	}
	cps, err := s.cfg.Store.ListCheckpoints(ctx)
	if err != nil {
		return err
	}
	category := make(map[store.Bib]string, len(runners))
	for _, r := range runners {
		category[r.Bib] = r.Category
	}
	cpName := make(map[string]string, len(cps))
	for _, c := range cps {
		cpName[c.Code] = c.Name
	}
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"bib", "category", "checkpoint", "checkpoint_name", "time_in", "source", "received_at", "count"})
	for _, e := range entries {
		source := e.SourceCall
		if source == "" {
			source = sourceLabelHQ
		}
		_ = cw.Write([]string{
			strconv.Itoa(int(e.Bib)), csvSafe(category[e.Bib]), e.CPCode, csvSafe(cpName[e.CPCode]),
			e.TimeIn.Format(time.RFC3339), csvSafe(source), e.ReceivedAt.Format(time.RFC3339), strconv.Itoa(e.Count),
		})
	}
	cw.Flush()
	return cw.Error()
}

// csvSafe neutralizes spreadsheet formula injection in free-text cells:
// a cell a spreadsheet would treat as a formula gets a leading quote.
func csvSafe(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}
