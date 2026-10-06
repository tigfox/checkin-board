package ops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"checkin-board/internal/journal"
	"checkin-board/internal/store"
)

func readJournal(t *testing.T, e *env) []journal.Record {
	t.Helper()
	f, err := os.Open(filepath.Join(e.dir, "race-journal.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	recs, skipped, err := journal.Parse(f)
	if err != nil || skipped != 0 {
		t.Fatalf("parse journal: %v (skipped %d)", err, skipped)
	}
	return recs
}

func TestLogBibJournalsAndStoresAtCheckpoint(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	r := e.logBib("AS5", 101)
	if r.ID == 0 || !r.TimeIn.Equal(t0) || !r.ClockSynced || r.JournalErr != nil {
		t.Fatalf("LogBib = %+v", r)
	}
	recs := readJournal(t, e)
	if len(recs) != 1 || recs[0].Event != journal.EventEntry || recs[0].Bib != 101 || recs[0].CP != "AS5" {
		t.Fatalf("journal = %+v", recs)
	}
	got, _ := e.svc.RecentEntries(ctx, 10)
	if len(got) != 1 || got[0].State != store.EntryQueued {
		t.Fatalf("recent = %+v", got)
	}
}

func TestLogBibGating(t *testing.T) {
	for _, st := range []string{store.RaceSetup, store.RaceComplete} {
		e := newEnv(t, checkpointSettings(), st)
		if _, err := e.svc.LogBib(ctx, "AS5", 1); !errors.Is(err, ErrWrongState) {
			t.Errorf("%s: err = %v, want ErrWrongState", st, err)
		}
	}
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	if _, err := e.svc.LogBib(ctx, "FIN", 1); !errors.Is(err, store.ErrInvalidInput) {
		t.Errorf("foreign code err = %v", err)
	}
	if _, err := e.svc.LogBib(ctx, "AS5", 0); !errors.Is(err, store.ErrInvalidInput) {
		t.Errorf("bib 0 err = %v", err)
	}
	unset := newEnv(t, store.DefaultSettings(), store.RaceSetup)
	if _, err := unset.svc.LogBib(ctx, "AS5", 1); !errors.Is(err, ErrWrongRole) {
		t.Errorf("no role err = %v", err)
	}
}

// failInserts makes every INSERT into table fail, through a second
// connection to the same database file.
func failInserts(t *testing.T, e *env, table string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(e.dir, "checkin-board.db")), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.Exec("CREATE TRIGGER fail_" + table + " BEFORE INSERT ON " + table +
		" BEGIN SELECT RAISE(ABORT, 'disk on fire'); END").Error; err != nil {
		t.Fatal(err)
	}
}

func TestLogBibKeepsJournalWhenDatabaseFails(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	failInserts(t, e, "local_entries")
	_, err := e.svc.LogBib(ctx, "AS5", 7)
	if !errors.Is(err, ErrJournalOnly) {
		t.Fatalf("err = %v, want ErrJournalOnly (tell the volunteer not to retype)", err)
	}
	if recs := readJournal(t, e); len(recs) != 1 || recs[0].Bib != 7 {
		t.Fatalf("journal = %+v, want the entry kept", recs)
	}
}

func TestLogBibPlainErrorWhenJournalAlsoFails(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	e.svc.cfg.JournalPath = filepath.Join(e.dir, "missing-dir", "j.csv")
	failInserts(t, e, "local_entries")
	_, err := e.svc.LogBib(ctx, "AS5", 7)
	if err == nil || errors.Is(err, ErrJournalOnly) {
		t.Fatalf("err = %v, want a plain error (nothing was kept)", err)
	}
}

func TestLogBibSurvivesJournalFailure(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	e.svc.cfg.JournalPath = filepath.Join(e.dir, "missing-dir", "j.csv")
	r := e.logBib("AS5", 7)
	if r.ID == 0 || r.JournalErr == nil {
		t.Fatalf("LogBib = %+v, want stored with a journal error", r)
	}
	if js := e.svc.JournalStatus(); !js.Enabled || js.LastError == "" || js.LastErrorAt == nil {
		t.Fatalf("journal status = %+v", js)
	}
}

func TestVoidBibCheckpoint(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	r := e.logBib("AS5", 101)
	if _, err := e.svc.VoidBib(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	recs := readJournal(t, e)
	if len(recs) != 2 || recs[1].Event != journal.EventVoid || !recs[1].TimeIn.Equal(r.TimeIn) {
		t.Fatalf("journal = %+v", recs)
	}
	if _, err := e.svc.VoidBib(ctx, 999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id err = %v", err)
	}
	if len(readJournal(t, e)) != 2 {
		t.Fatal("rejected void journaled")
	}
}

func TestHQKeypadLogAndVoid(t *testing.T) {
	e := newEnv(t, hqSettings(), store.RaceActive)
	r := e.logBib("FIN", 7)
	e.ft.Advance(time.Second)
	_ = e.logBib("FIN", 8)
	if _, err := e.svc.VoidBib(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.RecentEntries(ctx, 10)
	if err != nil || len(got) != 2 || got[0].Bib != 8 || !got[1].Voided || got[1].State != EntryRecorded {
		t.Fatalf("recent = %+v, %v", got, err)
	}
	if b, _ := e.svc.Board(ctx); len(b.Runners) != 1 || b.Runners[0].Bib != 8 {
		t.Fatalf("board after void = %+v", b.Runners)
	}
}

func TestVoidAllowedAfterCompleteNotAfterSecure(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	r := e.logBib("AS5", 1)
	if _, err := e.svc.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VoidBib(ctx, r.ID); err != nil {
		t.Fatalf("void after complete: %v", err)
	}
	r2 := e.logBibAfterReopen(t)
	if _, err := e.svc.Secure(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.VoidBib(ctx, r2); !errors.Is(err, ErrWrongState) {
		t.Fatalf("void while secured err = %v", err)
	}
}

// logBibAfterReopen stores an entry directly (the keypad is closed).
func (e *env) logBibAfterReopen(t *testing.T) uint {
	t.Helper()
	le, err := e.st.LogLocal(ctx, "AS5", 2, e.ft.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	return le.ID
}

func TestRecentEntriesNeedsRole(t *testing.T) {
	e := newEnv(t, store.DefaultSettings(), store.RaceSetup)
	if _, err := e.svc.RecentEntries(ctx, 10); !errors.Is(err, ErrWrongRole) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.VoidBib(ctx, 1); err == nil {
		t.Fatal("void without a role accepted")
	}
}

func TestJournalIsPlainText(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	e.logBib("AS5", 42)
	raw, err := os.ReadFile(filepath.Join(e.dir, "race-journal.csv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "entry,AS5,42,") {
		t.Fatalf("journal not human-readable: %q", raw)
	}
}
