package store

import (
	"bufio"
	"bytes"
	"checkin-board/internal/wire"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
)

// RowError is one rejected roster line.
type RowError struct {
	Line int
	Msg  string
}

// maxReportedRowErrors caps how many bad lines an import error lists;
// uploading the wrong file (a binary, an .xlsx) must not build a
// multi-megabyte error message.
const maxReportedRowErrors = 20

// maxRosterRows bounds a roster: bibs only go to 9999.
const maxRosterRows = wire.MaxBib + 1

// rowErrors collects bad lines up to maxReportedRowErrors, counting the rest.
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

func (r rowErrors) describe() string {
	parts := make([]string, len(r.Rows))
	for i, row := range r.Rows {
		parts[i] = fmt.Sprintf("line %d: %s", row.Line, row.Msg)
	}
	s := strings.Join(parts, "; ")
	if r.More > 0 {
		s += fmt.Sprintf("; and %d more", r.More)
	}
	return s
}

// RosterError lists the bad lines (the first maxReportedRowErrors of
// them) so the operator can fix the file in one pass. It wraps
// ErrInvalidInput.
type RosterError struct {
	Rows []RowError
	More int // bad lines beyond Rows
	// Reason, when set, rejects the file as a whole (e.g. too many rows).
	Reason string
}

func (e *RosterError) Error() string {
	switch {
	case e.Reason != "":
		return "store: invalid roster: " + e.Reason
	case len(e.Rows) == 0:
		return "store: roster has no runners"
	}
	return "store: invalid roster: " + rowErrors{Rows: e.Rows, More: e.More}.describe()
}

func (e *RosterError) Unwrap() error { return ErrInvalidInput }

// utf8BOM prefixes Excel's "CSV UTF-8" exports.
var utf8BOM = []byte("\xEF\xBB\xBF")

// ParseRosterCSV reads the roster: the bib in the first column and,
// only when a header row names one, a "category" column. Every other
// column (a registration export's name, gender, age, email, ...) is
// discarded: no personal data enters graywolf. A file without a header
// gives bibs only, because its second column might be a name. The
// header is recognized by a first cell of "bib" (first line only); a
// UTF-8 byte-order mark is ignored, bibs may carry leading zeros, and
// blank lines are ignored. The import is all or nothing: any bad line (or an empty
// roster) yields a *RosterError naming every bad line. Malformed CSV is
// reported per line, never a panic: this parses operator uploads.
func ParseRosterCSV(r io.Reader) ([]Runner, error) {
	br := bufio.NewReader(r)
	if head, err := br.Peek(len(utf8BOM)); err == nil && bytes.Equal(head, utf8BOM) {
		_, _ = br.Discard(len(utf8BOM))
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true

	var (
		runners []Runner
		bad     rowErrors
		seen    = map[Bib]int{} // bib -> first line
		first   = true
		rows    int
		catCol  = -1 // index of the "category" column; -1 = none
	)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		isFirst := first
		first = false
		if rows++; rows > maxRosterRows+1 { // +1 for a header
			return nil, &RosterError{Reason: fmt.Sprintf("more than %d rows", maxRosterRows)}
		}
		if err != nil {
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				bad.add(pe.Line, pe.Err.Error())
				continue
			}
			return nil, err
		}
		// FieldPos is only valid after a successful Read; it panics
		// after a ParseError.
		line, _ := cr.FieldPos(0)
		if isFirst && strings.EqualFold(strings.TrimSpace(rec[0]), "bib") {
			catCol = categoryColumn(rec)
			continue
		}
		runner, msg := parseRosterRecord(rec, catCol)
		if msg == "" {
			if prev, dup := seen[runner.Bib]; dup {
				msg = fmt.Sprintf("bib %d already listed on line %d", runner.Bib, prev)
			}
		}
		if msg != "" {
			bad.add(line, msg)
			continue
		}
		seen[runner.Bib] = line
		runners = append(runners, runner)
	}
	if !bad.empty() || len(runners) == 0 {
		return nil, &RosterError{Rows: bad.Rows, More: bad.More}
	}
	return runners, nil
}

// categoryColumn returns the index of the header cell "category"
// (case-insensitive), or -1.
func categoryColumn(header []string) int {
	for i, h := range header {
		if strings.EqualFold(strings.TrimSpace(h), "category") {
			return i
		}
	}
	return -1
}

// parseRosterRecord returns the runner or a message describing the
// problem. Only the bib (first column) and catCol are read.
func parseRosterRecord(rec []string, catCol int) (Runner, string) {
	bib, err := wire.ParseBib(rec[0])
	if err != nil {
		return Runner{}, fmt.Sprintf("bib %q must be 1-9999 (first column)", rec[0])
	}
	r := Runner{Bib: bib}
	if catCol > 0 && catCol < len(rec) {
		r.Category = strings.TrimSpace(rec[catCol])
	}
	if err := r.Validate(); err != nil {
		return Runner{}, strings.TrimPrefix(err.Error(), ErrInvalidInput.Error()+": ")
	}
	return r, ""
}
