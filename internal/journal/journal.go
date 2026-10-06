// Package journal is the bib journal: a plain-text, append-only record
// of every keypad action, kept beside checkin-board.db and independent of
// SQLite (spec 4.4). Each line is fsynced before the database write, so
// what a volunteer logged survives a power cut even if SQLite loses its
// last transactions or the DB file is damaged. It is readable by a
// person with a text editor, and HQ can import it. Ported from graywolf
// pkg/race (our own code).
package journal

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/wire"
)

// Journal event kinds.
const (
	EventEntry = "entry"
	EventVoid  = "void"
)

// journalHeader is the CSV header row; time columns are RFC 3339 UTC.
const journalHeader = "logged_at,event,cp,bib,time_in,clock_synced"

// Record is one journal line. TimeIn identifies the passage (it
// is the time stamped on the entry); a void repeats the voided entry's
// bib and TimeIn. LoggedAt is when the action happened.
type Record struct {
	LoggedAt    time.Time
	Event       string
	CP          string
	Bib         wire.Bib
	TimeIn      time.Time
	ClockSynced bool
}

// Journal appends records to the journal file. Safe for concurrent use.
type Journal struct {
	mu   sync.Mutex
	f    *os.File
	path string
	// dirty is set after a failed write, which may have left a partial
	// line; the next write starts on a fresh line so it isn't glued to
	// the fragment.
	dirty bool
}

var ErrClosed = errors.New("journal: journal closed")

// Open opens (creating if needed) the journal at path for
// appending, writing the header to a new file. Mode 0600: it holds race
// data, but nothing secret; it simply matches graywolf.db.
func Open(path string) (*Journal, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("journal: open journal: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("journal: stat journal: %w", err)
	}
	j := &Journal{f: f, path: path}
	if info.Size() == 0 {
		if err := j.writeSynced(journalHeader + "\n"); err != nil {
			_ = f.Close()
			return nil, err
		}
		// fsync on the file doesn't persist its directory entry: without
		// this, a power cut soon after creation can lose the whole file.
		syncDir(filepath.Dir(path))
		return j, nil
	}
	// A power cut may have torn the last line. Terminate it so the next
	// record starts on its own line instead of being glued to the
	// fragment (which Parse then skips).
	torn, err := endsWithoutNewline(path, info.Size())
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	j.dirty = torn
	return j, nil
}

func endsWithoutNewline(path string, size int64) (bool, error) {
	r, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("journal: read journal tail: %w", err)
	}
	defer r.Close()
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, size-1); err != nil {
		return false, fmt.Errorf("journal: read journal tail: %w", err)
	}
	return last[0] != '\n', nil
}

// syncDir fsyncs a directory so a newly created file's entry survives a
// power cut. Best effort: Windows can't fsync a directory handle, and
// failing to do so must not stop the journal from being used.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// Path returns the journal file path.
func (j *Journal) Path() string { return j.path }

// Append writes one record as a single line and fsyncs it.
func (j *Journal) Append(r Record) error {
	return j.writeSynced(formatJournalLine(r))
}

func (j *Journal) writeSynced(line string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return ErrClosed
	}
	if j.dirty {
		line = "\n" + line // blank lines are ignored when parsing
	}
	// One Write per line: a power cut can tear at most the last line.
	if _, err := j.f.WriteString(line); err != nil {
		j.dirty = true
		return fmt.Errorf("journal: write journal: %w", err)
	}
	if err := j.f.Sync(); err != nil {
		j.dirty = true
		return fmt.Errorf("journal: sync journal: %w", err)
	}
	j.dirty = false
	return nil
}

// Close closes the file. Idempotent.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.f == nil {
		return nil
	}
	err := j.f.Close()
	j.f = nil
	return err
}

func formatJournalLine(r Record) string {
	return strings.Join([]string{
		r.LoggedAt.UTC().Format(time.RFC3339),
		r.Event,
		r.CP,
		strconv.Itoa(int(r.Bib)),
		r.TimeIn.UTC().Format(time.RFC3339),
		strconv.FormatBool(r.ClockSynced),
	}, ",") + "\n"
}

// Parse reads a journal. Lines that don't parse (a header, a line
// torn by a power cut, hand-edit damage) are skipped and counted rather
// than failing the whole file: recovering most of a damaged journal
// beats recovering none of it.
//
// Lines are read without a length limit (a damaged region can be one
// enormous "line"), and a leading UTF-8 BOM from a hand edit is ignored.
func Parse(r io.Reader) (recs []Record, skipped int, err error) {
	br := bufio.NewReader(r)
	first := true
	for {
		raw, readErr := br.ReadString('\n')
		if first {
			raw = strings.TrimPrefix(raw, "\uFEFF")
			first = false
		}
		line := strings.TrimSpace(raw)
		if line != "" && line != journalHeader {
			if rec, ok := parseJournalLine(line); ok {
				recs = append(recs, rec)
			} else {
				skipped++
			}
		}
		if errors.Is(readErr, io.EOF) {
			return recs, skipped, nil
		}
		if readErr != nil {
			return recs, skipped, fmt.Errorf("journal: read journal: %w", readErr)
		}
	}
}

func parseJournalLine(line string) (Record, bool) {
	cr := csv.NewReader(strings.NewReader(line))
	cr.FieldsPerRecord = 6
	f, err := cr.Read()
	if err != nil {
		return Record{}, false
	}
	loggedAt, err1 := time.Parse(time.RFC3339, f[0])
	timeIn, err2 := time.Parse(time.RFC3339, f[4])
	bib, err3 := wire.ParseBib(f[3])
	// Exactly "true"/"false": ParseBool would accept "t", which is what
	// a line torn inside "true" looks like.
	if err := errors.Join(err1, err2, err3); err != nil || (f[5] != "true" && f[5] != "false") {
		return Record{}, false
	}
	synced := f[5] == "true"
	if (f[1] != EventEntry && f[1] != EventVoid) || !wire.ValidCheckpointCode(f[2]) {
		return Record{}, false
	}
	return Record{
		LoggedAt: loggedAt.UTC(), Event: f[1], CP: f[2], Bib: bib,
		TimeIn: timeIn.UTC(), ClockSynced: synced,
	}, true
}
