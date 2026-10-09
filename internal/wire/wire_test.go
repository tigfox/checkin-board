package wire

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func tod(h, m, s int) TimeOfDay { return TimeOfDay(h*3600 + m*60 + s) }

func TestParseBib(t *testing.T) {
	cases := []struct {
		in      string
		want    Bib
		wantErr bool
	}{
		{"1", 1, false},
		{"42", 42, false},
		{"0042", 42, false},
		{" 101 ", 101, false},
		{"9999", 9999, false},
		{"0", 0, true},
		{"0000", 0, true},
		{"10000", 0, true},
		{"", 0, true},
		{"12a", 0, true},
		{"-5", 0, true},
		{"+5", 0, true},
		{"1 2", 0, true},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, err := ParseBib(c.in)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidBib) {
					t.Fatalf("ParseBib(%q) err = %v, want ErrInvalidBib", c.in, err)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("ParseBib(%q) = %d, %v; want %d", c.in, got, err, c.want)
			}
		})
	}
}

func TestValidCheckpointCode(t *testing.T) {
	for _, ok := range []string{"3", "AS5", "FIN", "START", "CP1234"} {
		if !ValidCheckpointCode(ok) {
			t.Errorf("ValidCheckpointCode(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "fin", "TOOLONG", "A-1", "A 1", "É"} {
		if ValidCheckpointCode(bad) {
			t.Errorf("ValidCheckpointCode(%q) = true, want false", bad)
		}
	}
}

func TestTimeOfDayOf(t *testing.T) {
	loc := time.FixedZone("MDT", -6*3600)
	// 06:12:05 MDT is 12:12:05 UTC: the wire is always UTC.
	got := TimeOfDayOf(time.Date(2026, 10, 5, 6, 12, 5, 999, loc))
	if got != tod(12, 12, 5) {
		t.Fatalf("TimeOfDayOf = %d, want %d", got, tod(12, 12, 5))
	}
}

func TestTimeOfDayResolve(t *testing.T) {
	ref := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		tod  TimeOfDay
		ref  time.Time
		want time.Time
	}{
		{"same day earlier", tod(11, 58, 30), ref, time.Date(2026, 10, 5, 11, 58, 30, 0, time.UTC)},
		{"same day later", tod(12, 0, 4), ref, time.Date(2026, 10, 5, 12, 0, 4, 0, time.UTC)},
		{"logged before midnight, received after", tod(23, 59, 50),
			time.Date(2026, 10, 6, 0, 0, 20, 0, time.UTC), time.Date(2026, 10, 5, 23, 59, 50, 0, time.UTC)},
		{"sender clock slightly ahead across midnight", tod(0, 0, 10),
			time.Date(2026, 10, 5, 23, 59, 55, 0, time.UTC), time.Date(2026, 10, 6, 0, 0, 10, 0, time.UTC)},
		{"non-UTC ref is normalized", tod(12, 0, 0),
			time.Date(2026, 10, 5, 6, 0, 0, 0, time.FixedZone("MDT", -6*3600)), time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.tod.Resolve(c.ref); !got.Equal(c.want) {
				t.Fatalf("Resolve = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPackReport(t *testing.T) {
	entries := []Entry{
		{Bib: 101, Time: tod(8, 12, 5)},
		{Bib: 104, Time: tod(8, 12, 22)},
		{Bib: 57, Time: tod(8, 13, 2)},
		{Bib: 101, Time: tod(8, 12, 5), Void: true},
	}
	text, n, err := PackReport("3", 12, entries, DefaultMaxTextLen)
	if err != nil {
		t.Fatal(err)
	}
	want := "RC1 R 3 12 @0812 101/05 104/22 @0813 57/02 @0812 -101/05"
	if text != want || n != 4 {
		t.Fatalf("PackReport =\n %q (n=%d)\nwant\n %q (n=4)", text, n, want)
	}
}

func TestPackReportStopsAtMaxLen(t *testing.T) {
	var entries []Entry
	for i := 0; i < 20; i++ {
		entries = append(entries, Entry{Bib: Bib(1000 + i), Time: tod(9, 0, i)})
	}
	text, n, err := PackReport("AS5", 7, entries, DefaultMaxTextLen)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > DefaultMaxTextLen {
		t.Fatalf("len(text) = %d > %d: %q", len(text), DefaultMaxTextLen, text)
	}
	if n < 6 || n >= len(entries) {
		t.Fatalf("n = %d, want a partial batch of at least 6", n)
	}
	// The next entry must genuinely not have fit.
	longer, _, _ := PackReport("AS5", 7, entries[:n+1], 1000)
	if len(longer) <= DefaultMaxTextLen {
		t.Fatalf("entry %d would have fit (%d chars): %q", n, len(longer), longer)
	}
	// Larger frames (graywolf supports 200-char messages) carry more.
	_, nBig, err := PackReport("AS5", 7, entries, 200)
	if err != nil || nBig != len(entries) {
		t.Fatalf("maxLen 200: n=%d err=%v, want all %d", nBig, err, len(entries))
	}
}

func TestPackReportErrors(t *testing.T) {
	good := []Entry{{Bib: 1, Time: tod(1, 2, 3)}}
	cases := []struct {
		name    string
		cp      string
		seq     uint32
		entries []Entry
		maxLen  int
	}{
		{"no entries", "3", 1, nil, DefaultMaxTextLen},
		{"bad cp", "cp", 1, good, DefaultMaxTextLen},
		{"seq zero", "3", 0, good, DefaultMaxTextLen},
		{"bad bib", "3", 1, []Entry{{Bib: 0, Time: 1}}, DefaultMaxTextLen},
		{"bad bib high", "3", 1, []Entry{{Bib: 10000, Time: 1}}, DefaultMaxTextLen},
		{"bad time", "3", 1, []Entry{{Bib: 1, Time: 86400}}, DefaultMaxTextLen},
		{"maxLen too small", "3", 1, good, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := PackReport(c.cp, c.seq, c.entries, c.maxLen); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDecodeReport(t *testing.T) {
	got, err := Decode("RC1 R 3 12 @0812 101/05 104/22 @0813 57/02 -101/05")
	if err != nil {
		t.Fatal(err)
	}
	want := &Report{CP: "3", Seq: 12, Entries: []Entry{
		{Bib: 101, Time: tod(8, 12, 5)},
		{Bib: 104, Time: tod(8, 12, 22)},
		{Bib: 57, Time: tod(8, 13, 2)},
		{Bib: 101, Time: tod(8, 13, 5), Void: true},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeHeartbeat(t *testing.T) {
	got, err := Decode("RC1 H FIN 0 235959")
	if err != nil {
		t.Fatal(err)
	}
	want := &Heartbeat{CP: "FIN", LastSeq: 0, Time: tod(23, 59, 59)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeGap(t *testing.T) {
	got, err := Decode("RC1 G 3 3,12,14-16")
	if err != nil {
		t.Fatal(err)
	}
	want := &GapRequest{CP: "3", Seqs: []uint32{3, 12, 14, 15, 16}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeErrors(t *testing.T) {
	bad := []string{
		"",
		"hello",
		"RC1",
		"RC1 ",
		"RC2 R 3 1 @0000 1/00",
		"rc1 R 3 1 @0000 1/00",
		"RC1 X 3 1",
		"RC1 R 3 1",                       // no entries
		"RC1 R 3 1 @0812",                 // group with no entries
		"RC1 R 3 1 101/05",                // entry before any group
		"RC1 R 3 0 @0812 101/05",          // seq 0
		"RC1 R cp 1 @0812 101/05",         // bad cp
		"RC1 R 3 x @0812 101/05",          // bad seq
		"RC1 R 3 1 @2400 101/05",          // hour out of range
		"RC1 R 3 1 @0860 101/05",          // minute out of range
		"RC1 R 3 1 @812 101/05",           // short group
		"RC1 R 3 1 @0812 101/60",          // seconds out of range
		"RC1 R 3 1 @0812 101/5",           // seconds not 2 digits
		"RC1 R 3 1 @0812 0101/05",         // non-canonical bib
		"RC1 R 3 1 @0812 0/05",            // bib zero
		"RC1 R 3 1 @0812 10000/05",        // bib too long
		"RC1 R 3 1 @0812 101",             // missing seconds
		"RC1 R 3 1 @0812 --101/05",        // double void
		"RC1 R 3 99999999999 @0812 1/05",  // seq overflow
		"RC1 H 3 1",                       // missing time
		"RC1 H 3 1 2400000",               // bad time
		"RC1 H 3 1 120000 extra",          // trailing token
		"RC1 G 3",                         // no seqs
		"RC1 G 3 0",                       // seq zero
		"RC1 G 3 5-3",                     // inverted range
		"RC1 G 3 1,,2",                    // empty element
		"RC1 G 3 1-2000000",               // range too large
		"RC1 G 3 1 2",                     // trailing token
		"RC1 R  3 12 @0812 101/05",        // double space
		"RC1 R 3 12 @0812 101/05 ",        // trailing space
		"RC1 R 3 12\t@0812 101/05",        // tab separator
		"RC1 R 3 012 @0812 101/05",        // leading-zero seq
		"RC1 R 3 1 @0812 1/05 @0812 2/06", // redundant group
		"RC1 H 3 007 120000",              // leading-zero lastseq
		"RC1 G 3 5,3",                     // not ascending
		"RC1 G 3 3,3",                     // duplicate
		"RC1 G 3 3,4",                     // adjacent singles must be a range
		"RC1 G 3 1-5,3",                   // overlapping
		"RC1 G 3 1-3,4",                   // adjacent to range
		"RC1 G 3 5-5",                     // degenerate range
		"RC1 G 3 03",                      // leading-zero seq
	}
	for _, in := range bad {
		t.Run(in, func(t *testing.T) {
			if _, err := Decode(in); !errors.Is(err, ErrDecode) {
				t.Fatalf("Decode(%q) err = %v, want ErrDecode", in, err)
			}
		})
	}
}

func TestIsRaceText(t *testing.T) {
	if !IsRaceText("RC1 R 3 1 @0000 1/00") {
		t.Fatal("expected true")
	}
	for _, s := range []string{"", "RC1", "RC10 R", "hello RC1 ", "@@#RC1"} {
		if IsRaceText(s) {
			t.Fatalf("IsRaceText(%q) = true", s)
		}
	}
}

func TestEncodeHeartbeat(t *testing.T) {
	got, err := EncodeHeartbeat(Heartbeat{CP: "AS5", LastSeq: 41, Time: tod(7, 5, 9)})
	if err != nil {
		t.Fatal(err)
	}
	if got != "RC1 H AS5 41 070509" {
		t.Fatalf("got %q", got)
	}
	if _, err := EncodeHeartbeat(Heartbeat{CP: "bad", Time: 1}); err == nil {
		t.Fatal("expected error for bad cp")
	}
	if _, err := EncodeHeartbeat(Heartbeat{CP: "3", Time: -1}); err == nil {
		t.Fatal("expected error for bad time")
	}
}

func TestPackGap(t *testing.T) {
	text, rest, err := PackGap("3", []uint32{16, 3, 12, 14, 15, 12, 4}, DefaultMaxTextLen)
	if err != nil {
		t.Fatal(err)
	}
	if text != "RC1 G 3 3-4,12,14-16" || len(rest) != 0 {
		t.Fatalf("got %q rest=%v", text, rest)
	}
}

func TestPackGapOverflowReturnsRest(t *testing.T) {
	var seqs []uint32
	for i := uint32(1); i <= 60; i += 2 { // 30 isolated seqs, no ranges
		seqs = append(seqs, i*1000)
	}
	text, rest, err := PackGap("3", seqs, DefaultMaxTextLen)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > DefaultMaxTextLen || len(rest) == 0 {
		t.Fatalf("len=%d rest=%d: %q", len(text), len(rest), text)
	}
	g, err := Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	sent := g.(*GapRequest).Seqs
	if len(sent)+len(rest) != len(seqs) {
		t.Fatalf("sent %d + rest %d != %d", len(sent), len(rest), len(seqs))
	}
	if rest[0] <= sent[len(sent)-1] {
		t.Fatalf("rest must continue after sent: sent ends %d, rest starts %d", sent[len(sent)-1], rest[0])
	}
}

func TestPackGapErrors(t *testing.T) {
	if _, _, err := PackGap("3", nil, DefaultMaxTextLen); err == nil {
		t.Fatal("expected error for empty seqs")
	}
	if _, _, err := PackGap("3", []uint32{0}, DefaultMaxTextLen); err == nil {
		t.Fatal("expected error for seq 0")
	}
	if _, _, err := PackGap("x", []uint32{1}, DefaultMaxTextLen); err == nil {
		t.Fatal("expected error for bad cp")
	}
}

func TestPackGapDoesNotMutateInput(t *testing.T) {
	in := []uint32{5, 1, 3}
	if _, _, err := PackGap("3", in, DefaultMaxTextLen); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, []uint32{5, 1, 3}) {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestRoundTripReport(t *testing.T) {
	entries := []Entry{
		{Bib: 9999, Time: tod(23, 59, 59)},
		{Bib: 1, Time: tod(0, 0, 0)},
		{Bib: 42, Time: tod(0, 0, 0), Void: true},
	}
	text, n, err := PackReport("START", 4000000, entries, 200)
	if err != nil || n != len(entries) {
		t.Fatalf("pack: n=%d err=%v", n, err)
	}
	got, err := Decode(text)
	if err != nil {
		t.Fatalf("decode %q: %v", text, err)
	}
	want := &Report{CP: "START", Seq: 4000000, Entries: entries}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("\n got: %+v\nwant: %+v", got, want)
	}
}

// FuzzDecode: Decode never panics, and anything it accepts re-encodes to
// the byte-identical string (the decoder is text-canonical).
func FuzzDecode(f *testing.F) {
	for _, s := range []string{
		"RC1 R 3 12 @0812 101/05 104/22 @0813 57/02 -101/05",
		"RC1 H FIN 0 235959",
		"RC1 G 3 3,12,14-16",
		"RC1 R 3 1 @0812",
		"RC1 G 3 1-2000000",
		"RC1 P AS5 12 3/5",
		"RC1 Q AS5 12 1-3,5 -23 -",
		"RC1 Q FIN 1 4 X RIDGE-1",
		"hello",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		msg, err := Decode(s)
		if err != nil {
			if msg != nil {
				t.Fatalf("non-nil message with error for %q", s)
			}
			return
		}
		var re string
		switch m := msg.(type) {
		case *Report:
			var n int
			re, n, err = PackReport(m.CP, m.Seq, m.Entries, 1<<16)
			if err == nil && n != len(m.Entries) {
				t.Fatalf("re-pack dropped entries for %q", s)
			}
		case *Heartbeat:
			re, err = EncodeHeartbeat(*m)
		case *GapRequest:
			var rest []uint32
			re, rest, err = PackGap(m.CP, m.Seqs, 1<<20)
			if err == nil && len(rest) != 0 {
				t.Fatalf("re-pack left rest for %q", s)
			}
		case *Probe:
			re, err = EncodeProbe(*m)
		case *ProbeReply:
			re, err = EncodeProbeReply(*m)
		default:
			t.Fatalf("unknown message type %T", msg)
		}
		if err != nil {
			t.Fatalf("accepted %q but re-encode failed: %v", s, err)
		}
		if re != s {
			t.Fatalf("accepted non-canonical %q; canonical form is %q", s, re)
		}
	})
}

// A closed checkpoint (4.7) marks its heartbeats with a trailing " C".
func TestHeartbeatClosedFlag(t *testing.T) {
	text, err := EncodeHeartbeat(Heartbeat{CP: "AS5", LastSeq: 41, Time: tod(7, 5, 9), Closed: true})
	if err != nil || text != "RC1 H AS5 41 070509 C" {
		t.Fatalf("encode = %q, %v", text, err)
	}
	m, err := Decode(text)
	if err != nil {
		t.Fatal(err)
	}
	if hb := m.(*Heartbeat); !hb.Closed || hb.LastSeq != 41 {
		t.Fatalf("decode = %+v", hb)
	}
	open, _ := Decode("RC1 H AS5 41 070509")
	if open.(*Heartbeat).Closed {
		t.Fatal("heartbeat without the flag decoded as closed")
	}
	for _, bad := range []string{"RC1 H AS5 41 070509 X", "RC1 H AS5 41 070509 C C", "RC1 H AS5 41 070509 c"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
