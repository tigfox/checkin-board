package journal

import (
	"checkin-board/internal/wire"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func journalRec(bib wire.Bib, at time.Time, event string) Record {
	return Record{LoggedAt: at, Event: event, CP: "AS5", Bib: bib, TimeIn: at, ClockSynced: true}
}

func TestJournalAppendAndParse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "race-journal.csv")
	j, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		journalRec(101, t0, EventEntry),
		journalRec(102, at(5*time.Second), EventEntry),
		journalRec(101, t0, EventVoid),
	}
	want[1].ClockSynced = false
	for _, r := range want {
		if err := j.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening appends; the header is written only once.
	j, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	extra := journalRec(7, at(time.Minute), EventEntry)
	if err := j.Append(extra); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	want = append(want, extra)

	raw, _ := os.ReadFile(path)
	if n := strings.Count(string(raw), journalHeader); n != 1 {
		t.Fatalf("header appears %d times", n)
	}
	f, _ := os.Open(path)
	defer f.Close()
	got, skipped, err := Parse(f)
	if err != nil || skipped != 0 {
		t.Fatalf("Parse: skipped %d, err %v", skipped, err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestJournalFileIsHumanReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.csv")
	j, _ := Open(path)
	_ = j.Append(journalRec(101, t0, EventEntry))
	_ = j.Close()
	raw, _ := os.ReadFile(path)
	want := journalHeader + "\n2026-10-10T13:00:00Z,entry,AS5,101,2026-10-10T13:00:00Z,true\n"
	if string(raw) != want {
		t.Fatalf("journal =\n%s\nwant\n%s", raw, want)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %v, want 0600", info.Mode().Perm())
	}
}

// A power cut mid-write leaves a torn last line: skip it, keep the rest.
func TestParseJournalSkipsTornLines(t *testing.T) {
	in := journalHeader + "\n" +
		"2026-10-10T13:00:00Z,entry,AS5,101,2026-10-10T13:00:00Z,true\n" +
		"2026-10-10T13:00:05Z,entry,AS5,10" // torn
	got, skipped, err := Parse(strings.NewReader(in))
	if err != nil || len(got) != 1 || skipped != 1 || got[0].Bib != 101 {
		t.Fatalf("got %+v, skipped %d, err %v", got, skipped, err)
	}
}

func TestParseJournalSkipsGarbage(t *testing.T) {
	in := journalHeader + "\n" +
		"not,a,valid,row\n" +
		"2026-10-10T13:00:00Z,teleport,AS5,101,2026-10-10T13:00:00Z,true\n" +
		"2026-10-10T13:00:00Z,entry,as5,101,2026-10-10T13:00:00Z,true\n" +
		"2026-10-10T13:00:00Z,entry,AS5,0,2026-10-10T13:00:00Z,true\n" +
		"2026-10-10T13:00:00Z,entry,AS5,101,yesterday,true\n" +
		"2026-10-10T13:00:00Z,entry,AS5,101,2026-10-10T13:00:00Z,maybe\n" +
		"2026-10-10T13:00:00Z,entry,AS5,102,2026-10-10T13:00:00Z,false\n"
	got, skipped, err := Parse(strings.NewReader(in))
	if err != nil || len(got) != 1 || got[0].Bib != 102 || skipped != 6 {
		t.Fatalf("got %+v, skipped %d, err %v", got, skipped, err)
	}
}

func TestParseJournalWithoutHeader(t *testing.T) {
	got, _, err := Parse(strings.NewReader("2026-10-10T13:00:00Z,entry,AS5,101,2026-10-10T13:00:00Z,true\n"))
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

func TestOpenJournalFailsOnBadPath(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "missing-dir", "j.csv")); err == nil {
		t.Fatal("expected an error for a missing directory")
	}
}

func TestJournalConcurrentAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.csv")
	j, _ := Open(path)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = j.Append(journalRec(wire.Bib(i+1), t0, EventEntry))
		}(i)
	}
	wg.Wait()
	_ = j.Close()
	f, _ := os.Open(path)
	defer f.Close()
	got, skipped, _ := Parse(f)
	if len(got) != 50 || skipped != 0 {
		t.Fatalf("got %d records, %d skipped; lines interleaved?", len(got), skipped)
	}
}

func TestJournalAppendAfterClose(t *testing.T) {
	j, _ := Open(filepath.Join(t.TempDir(), "j.csv"))
	_ = j.Close()
	if err := j.Append(journalRec(1, t0, EventEntry)); err == nil {
		t.Fatal("append after close succeeded")
	}
	if err := j.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

// H1: after a power cut tears the last line, the next append must not
// be glued onto the fragment.
func TestJournalRecoversFromTornTail(t *testing.T) {
	for name, tail := range map[string]string{
		"torn entry":  journalHeader + "\n2026-10-10T13:00:00Z,entry,AS5,10",
		"torn header": "logged_at,event,c",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "j.csv")
			if err := os.WriteFile(path, []byte(tail), 0o600); err != nil {
				t.Fatal(err)
			}
			j, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := j.Append(journalRec(7, t0, EventEntry)); err != nil {
				t.Fatal(err)
			}
			_ = j.Close()
			f, _ := os.Open(path)
			defer f.Close()
			got, _, _ := Parse(f)
			if len(got) != 1 || got[0].Bib != 7 {
				t.Fatalf("records after torn tail = %+v, want the new entry intact", got)
			}
		})
	}
}

// H1: a failed write leaves a partial line; the next line starts fresh.
func TestJournalStartsFreshLineAfterWriteError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.csv")
	j, _ := Open(path)
	good := j.f
	ro, _ := os.Open(path) // read-only handle: writes fail
	j.f = ro
	if err := j.Append(journalRec(1, t0, EventEntry)); err == nil {
		t.Fatal("expected a write error")
	}
	_ = ro.Close()
	// Simulate the failed write having left a fragment on disk.
	_, _ = good.WriteString("2026-10-10T13:00:00Z,entry,AS5,1")
	j.f = good
	if err := j.Append(journalRec(2, t0, EventEntry)); err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	f, _ := os.Open(path)
	defer f.Close()
	got, skipped, _ := Parse(f)
	if len(got) != 1 || got[0].Bib != 2 || skipped != 1 {
		t.Fatalf("got %+v skipped %d, want bib 2 intact and the fragment skipped", got, skipped)
	}
}

func TestParseJournalHandlesHugeGarbage(t *testing.T) {
	in := strings.Repeat("\x00", 200_000) + "\n" +
		"2026-10-10T13:00:00Z,entry,AS5,9,2026-10-10T13:00:00Z,true\n"
	got, skipped, err := Parse(strings.NewReader(in))
	if err != nil || len(got) != 1 || skipped != 1 {
		t.Fatalf("got %d, skipped %d, err %v", len(got), skipped, err)
	}
}

func TestParseJournalStrictBoolAndBOM(t *testing.T) {
	in := "\xEF\xBB\xBF" + journalHeader + "\n" +
		"2026-10-10T13:00:00Z,entry,AS5,9,2026-10-10T13:00:00Z,t\n" + // torn inside "true"
		"2026-10-10T13:00:00Z,entry,AS5,9,2026-10-10T13:00:00Z,false\n"
	got, skipped, _ := Parse(strings.NewReader(in))
	if len(got) != 1 || skipped != 1 {
		t.Fatalf("got %d records, skipped %d; want BOM header ignored and \"t\" rejected", len(got), skipped)
	}
}

// t0 is a fixed race-morning instant.
var t0 = time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return t0.Add(d) }
