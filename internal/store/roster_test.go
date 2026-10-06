package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCheckpointCRUD(t *testing.T) {
	s := newTestStore(t)
	cps := []*Checkpoint{
		{Code: "FIN", Name: "Finish", CourseOrder: 9},
		{Code: "AS1", Name: "Ridge Aid", CourseOrder: 1, ExpectedCall: "KK7ABC-7"},
		{Code: "START", Name: "Start", CourseOrder: 0},
	}
	for _, c := range cps {
		if err := s.CreateCheckpoint(ctx, c); err != nil {
			t.Fatalf("create %s: %v", c.Code, err)
		}
	}
	list, err := s.ListCheckpoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 || list[0].Code != "START" || list[1].Code != "AS1" || list[2].Code != "FIN" {
		t.Fatalf("list not in course order: %+v", list)
	}

	if err := s.CreateCheckpoint(ctx, &Checkpoint{Code: "AS1", Name: "Dup"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("dup code err = %v, want ErrConflict", err)
	}

	upd := *cps[1]
	upd.Name = "Ridge Aid (moved)"
	upd.CourseOrder = 2
	if err := s.UpdateCheckpoint(ctx, &upd); err != nil {
		t.Fatal(err)
	}
	list, _ = s.ListCheckpoints(ctx)
	if list[1].Name != "Ridge Aid (moved)" || list[1].CourseOrder != 2 {
		t.Fatalf("update not applied: %+v", list[1])
	}
	if err := s.UpdateCheckpoint(ctx, &Checkpoint{ID: 999, Code: "X", Name: "X"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update missing err = %v", err)
	}

	if err := s.DeleteCheckpoint(ctx, cps[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteCheckpoint(ctx, cps[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
	list, _ = s.ListCheckpoints(ctx)
	if len(list) != 2 {
		t.Fatalf("len after delete = %d", len(list))
	}
}

func TestCheckpointValidate(t *testing.T) {
	cases := []struct {
		name string
		c    Checkpoint
		ok   bool
	}{
		{"ok", Checkpoint{Code: "AS1", Name: "Aid"}, true},
		{"bad code", Checkpoint{Code: "as1", Name: "Aid"}, false},
		{"empty name", Checkpoint{Code: "AS1", Name: " "}, false},
		{"long name", Checkpoint{Code: "AS1", Name: strings.Repeat("x", 65)}, false},
		{"negative order", Checkpoint{Code: "AS1", Name: "Aid", CourseOrder: -1}, false},
		{"bad call", Checkpoint{Code: "AS1", Name: "Aid", ExpectedCall: "not a call"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.c.Validate()
			if c.ok != (err == nil) {
				t.Fatalf("Validate = %v, want ok=%v", err, c.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("err = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestUpsertRunners(t *testing.T) {
	s := newTestStore(t)
	n, err := s.UpsertRunners(ctx, []Runner{{Bib: 7, Category: "50K"}, {Bib: 12}})
	if err != nil || n != 2 {
		t.Fatalf("UpsertRunners = %d, %v", n, err)
	}
	// Re-import updates by bib.
	if _, err := s.UpsertRunners(ctx, []Runner{{Bib: 7, Category: "100K"}}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetRunner(ctx, 7); err != nil || r.Category != "100K" {
		t.Fatalf("GetRunner = %+v, %v", r, err)
	}
	list, _ := s.ListRunners(ctx)
	if len(list) != 2 || list[0].Bib != 7 || list[1].Bib != 12 {
		t.Fatalf("ListRunners = %+v", list)
	}
	if _, err := s.GetRunner(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing runner err = %v", err)
	}
	if err := s.DeleteRunner(ctx, 12); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRunner(ctx, 12); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete missing err = %v", err)
	}
}

func TestUpsertRunnersAllOrNothing(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpsertRunners(ctx, []Runner{{Bib: 1}, {Bib: 0}})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	if list, _ := s.ListRunners(ctx); len(list) != 0 {
		t.Fatalf("partial import persisted: %+v", list)
	}
}

// No PII enters graywolf: a registration export may carry names,
// gender, age, email... Only the bib and a column headed "category"
// are kept; everything else is discarded.
func TestParseRosterCSVKeepsOnlyBibAndCategory(t *testing.T) {
	in := "Bib,First,Last,Gender,Category,Email\n0042,Ann,Lee,F,50K,a@x\n7,Bo,Diaz,M,100K,b@y\n\n12,Cy,Ng,X,,c@z\n"
	got, err := ParseRosterCSV(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	want := []Runner{{Bib: 42, Category: "50K"}, {Bib: 7, Category: "100K"}, {Bib: 12}}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i].Bib != want[i].Bib || got[i].Category != want[i].Category {
			t.Fatalf("row %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Without a header naming the category column, a second column might
// be a name, so nothing but the bib is taken.
func TestParseRosterCSVHeaderlessIsBibsOnly(t *testing.T) {
	got, err := ParseRosterCSV(strings.NewReader("7,Ann Lee\n8,Bo Diaz\n"))
	if err != nil || len(got) != 2 || got[0].Category != "" || got[1].Category != "" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseRosterCSVRejectsBadCategory(t *testing.T) {
	_, err := ParseRosterCSV(strings.NewReader("bib,category\n7,ok\n8,bad\x01cat\n9," + strings.Repeat("x", MaxCategoryLen+1) + "\n"))
	var re *RosterError
	if !errors.As(err, &re) || len(re.Rows) != 2 || re.Rows[0].Line != 3 || re.Rows[1].Line != 4 {
		t.Fatalf("err = %v", err)
	}
}

func TestRunnerTableHasNoPersonalColumns(t *testing.T) {
	s := newTestStore(t)
	var cols []struct{ Name string }
	if err := s.db.Raw("SELECT name FROM pragma_table_info('runners')").Scan(&cols).Error; err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		switch c.Name {
		case "id", "bib", "category", "created_at", "updated_at":
		default:
			t.Errorf("runners has column %q; the roster holds no personal data", c.Name)
		}
	}
}

func TestParseRosterCSVNoHeader(t *testing.T) {
	got, err := ParseRosterCSV(strings.NewReader("5\n"))
	if err != nil || len(got) != 1 || got[0].Bib != 5 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseRosterCSVReportsEveryBadLine(t *testing.T) {
	in := "bib,name\n1,Ok\nabc,Bad bib\n2,\n1,Dup bib\n3,Extra,columns,ignored\n0,Zero\n"
	_, err := ParseRosterCSV(strings.NewReader(in))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput", err)
	}
	var re *RosterError
	if !errors.As(err, &re) {
		t.Fatalf("err %T is not *RosterError", err)
	}
	var lines []int
	for _, row := range re.Rows {
		lines = append(lines, row.Line)
	}
	if want := []int{3, 5, 7}; len(lines) != len(want) || lines[0] != 3 || lines[1] != 5 || lines[2] != 7 {
		t.Fatalf("bad lines = %v, want %v", lines, want)
	}
}

func TestParseRosterCSVEmpty(t *testing.T) {
	if _, err := ParseRosterCSV(strings.NewReader("bib,name\n")); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want ErrInvalidInput for empty roster", err)
	}
}

func TestRosterErrorMessage(t *testing.T) {
	_, err := ParseRosterCSV(strings.NewReader("1,Ok\nabc,Bad\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2:") || !strings.Contains(err.Error(), `"abc"`) {
		t.Fatalf("err = %v, want line number and bad value", err)
	}
	_, err = ParseRosterCSV(strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no runners") {
		t.Fatalf("empty err = %v", err)
	}
}

// Malformed CSV from an operator upload must be reported, never panic.
func TestParseRosterCSVMalformedQuotesDoNotPanic(t *testing.T) {
	for _, in := range []string{
		"\"a\nb",               // unterminated quote
		"bib,name\n\"x\"y,z\n", // bare quote mid-field after a valid header
		"1,\"ab\"c\n",          // bare quote on the first line
	} {
		_, err := ParseRosterCSV(strings.NewReader(in))
		if !errors.Is(err, ErrInvalidInput) {
			t.Errorf("ParseRosterCSV(%q) err = %v, want ErrInvalidInput", in, err)
		}
	}
}

// Excel's "CSV UTF-8" export starts with a byte-order mark.
func TestParseRosterCSVStripsBOM(t *testing.T) {
	got, err := ParseRosterCSV(strings.NewReader("\xEF\xBB\xBFbib,name\n7,Al\n"))
	if err != nil || len(got) != 1 || got[0].Bib != 7 {
		t.Fatalf("with header: %+v, %v", got, err)
	}
	got, err = ParseRosterCSV(strings.NewReader("\xEF\xBB\xBF7,Al\n"))
	if err != nil || len(got) != 1 || got[0].Bib != 7 {
		t.Fatalf("without header: %+v, %v", got, err)
	}
}

// Only a header on the very first line is skipped; a "bib" row later is an error.
func TestParseRosterCSVHeaderOnlyOnFirstLine(t *testing.T) {
	_, err := ParseRosterCSV(strings.NewReader("1,Al\nbib,name\n"))
	var re *RosterError
	if !errors.As(err, &re) || len(re.Rows) != 1 || re.Rows[0].Line != 2 {
		t.Fatalf("err = %v, want line 2 rejected", err)
	}
}

// FuzzParseRosterCSV: operator uploads must never panic the parser, and
// any accepted roster must contain only valid runners.
func FuzzParseRosterCSV(f *testing.F) {
	for _, s := range []string{
		"bib,name,category\n0042,Ann Lee,F40\n7,\"Diaz, Bo\"\n",
		"\"a\nb", "bib,name\n\"x\"y,z\n", "\xEF\xBB\xBFbib,name\n7,Al\n", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		runners, err := ParseRosterCSV(strings.NewReader(s))
		if err != nil {
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("unexpected error type for %q: %v", s, err)
			}
			return
		}
		for _, r := range runners {
			if err := r.Validate(); err != nil {
				t.Fatalf("accepted invalid runner %+v from %q", r, s)
			}
		}
	})
}

func TestParseRosterCSVCapsErrorList(t *testing.T) {
	in := strings.Repeat("abc,Bad\n", 5000)
	_, err := ParseRosterCSV(strings.NewReader(in))
	var re *RosterError
	if !errors.As(err, &re) || len(re.Rows) != maxReportedRowErrors || re.More != 5000-maxReportedRowErrors {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "and 4980 more") || len(err.Error()) > 4000 {
		t.Fatalf("message = %d bytes: %.200s", len(err.Error()), err.Error())
	}
}

func TestParseRosterCSVCapsRows(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= maxRosterRows+1; i++ {
		fmt.Fprintf(&b, "%d,R%d\n", i%9999+1, i)
	}
	if _, err := ParseRosterCSV(strings.NewReader(b.String())); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("err = %v, want a row-cap error", err)
	}
}

func TestCheckpointNameRejectsControlCharacters(t *testing.T) {
	for _, name := range []string{"Aid\nTwo", "Aid\x00", "Aid\u202eTwo"} {
		if err := (Checkpoint{Code: "A", Name: name}).Validate(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("checkpoint name %q accepted", name)
		}
	}
	if err := (Checkpoint{Code: "A", Name: "Peña Ridge Aid"}).Validate(); err != nil {
		t.Errorf("legit name rejected: %v", err)
	}
}
