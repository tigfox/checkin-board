package wire

import (
	"errors"
	"reflect"
	"testing"
)

func TestEncodeProbe(t *testing.T) {
	cases := []struct {
		in   Probe
		want string
	}{
		{Probe{CP: "AS5", Run: 12, Index: 1, Total: 5}, "RC1 P AS5 12 1/5"},
		{Probe{CP: "FIN", Run: 9999, Index: 20, Total: 20}, "RC1 P FIN 9999 20/20"},
	}
	for _, c := range cases {
		got, err := EncodeProbe(c.in)
		if err != nil || got != c.want {
			t.Errorf("EncodeProbe(%+v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestEncodeProbeErrors(t *testing.T) {
	bad := []Probe{
		{CP: "as5", Run: 1, Index: 1, Total: 5},
		{CP: "AS5", Run: 0, Index: 1, Total: 5},
		{CP: "AS5", Run: MaxProbeRun + 1, Index: 1, Total: 5},
		{CP: "AS5", Run: 1, Index: 0, Total: 5},
		{CP: "AS5", Run: 1, Index: 6, Total: 5},
		{CP: "AS5", Run: 1, Index: 1, Total: 0},
		{CP: "AS5", Run: 1, Index: 1, Total: MaxProbes + 1},
	}
	for _, p := range bad {
		if _, err := EncodeProbe(p); !errors.Is(err, ErrEncode) {
			t.Errorf("EncodeProbe(%+v) err = %v, want ErrEncode", p, err)
		}
	}
}

func TestEncodeProbeReply(t *testing.T) {
	cases := []struct {
		in   ProbeReply
		want string
	}{
		{ProbeReply{CP: "AS5", Run: 12, Heard: []uint8{1, 2, 3, 5}, Level: -23, Via: ""}, "RC1 Q AS5 12 1-3,5 -23 -"},
		{ProbeReply{CP: "AS5", Run: 12, Heard: []uint8{4}, Level: LevelUnknown, Via: "RIDGE-1"}, "RC1 Q AS5 12 4 X RIDGE-1"},
		{ProbeReply{CP: "3", Run: 1, Heard: []uint8{5, 1, 3, 2, 4}, Level: 0, Via: "N0CALL"}, "RC1 Q 3 1 1-5 0 N0CALL"},
		{ProbeReply{CP: "3", Run: 1, Heard: []uint8{2, 2}, Level: MinLevel}, "RC1 Q 3 1 2 -99 -"},
	}
	for _, c := range cases {
		got, err := EncodeProbeReply(c.in)
		if err != nil || got != c.want {
			t.Errorf("EncodeProbeReply(%+v) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}

func TestEncodeProbeReplyErrors(t *testing.T) {
	ok := ProbeReply{CP: "AS5", Run: 1, Heard: []uint8{1}, Level: -20}
	mut := func(f func(*ProbeReply)) ProbeReply {
		r := ok
		r.Heard = append([]uint8(nil), ok.Heard...)
		f(&r)
		return r
	}
	bad := []ProbeReply{
		mut(func(r *ProbeReply) { r.CP = "" }),
		mut(func(r *ProbeReply) { r.Run = 0 }),
		mut(func(r *ProbeReply) { r.Heard = nil }),
		mut(func(r *ProbeReply) { r.Heard = []uint8{0} }),
		mut(func(r *ProbeReply) { r.Heard = []uint8{MaxProbes + 1} }),
		mut(func(r *ProbeReply) { r.Level = MinLevel - 1 }),
		mut(func(r *ProbeReply) { r.Level = 2 }),
		mut(func(r *ProbeReply) { r.Via = "bad call" }),
		mut(func(r *ProbeReply) { r.Via = "-" }),
	}
	for _, r := range bad {
		if _, err := EncodeProbeReply(r); !errors.Is(err, ErrEncode) {
			t.Errorf("EncodeProbeReply(%+v) err = %v, want ErrEncode", r, err)
		}
	}
}

func TestEncodeProbeReplyDoesNotMutateInput(t *testing.T) {
	in := []uint8{5, 1, 3}
	if _, err := EncodeProbeReply(ProbeReply{CP: "A", Run: 1, Heard: in, Level: LevelUnknown}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(in, []uint8{5, 1, 3}) {
		t.Fatalf("input mutated: %v", in)
	}
}

func TestDecodeProbe(t *testing.T) {
	got, err := Decode("RC1 P AS5 12 3/5")
	if err != nil {
		t.Fatal(err)
	}
	want := &Probe{CP: "AS5", Run: 12, Index: 3, Total: 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDecodeProbeReply(t *testing.T) {
	cases := []struct {
		in   string
		want *ProbeReply
	}{
		{"RC1 Q AS5 12 1-3,5 -23 -", &ProbeReply{CP: "AS5", Run: 12, Heard: []uint8{1, 2, 3, 5}, Level: -23}},
		{"RC1 Q AS5 12 4 X RIDGE-1", &ProbeReply{CP: "AS5", Run: 12, Heard: []uint8{4}, Level: LevelUnknown, Via: "RIDGE-1"}},
		{"RC1 Q 3 9999 1-20 0 WIDE2-1", &ProbeReply{CP: "3", Run: 9999, Heard: []uint8{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, Level: 0, Via: "WIDE2-1"}},
	}
	for _, c := range cases {
		got, err := Decode(c.in)
		if err != nil {
			t.Fatalf("Decode(%q): %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("Decode(%q)\n got %+v\nwant %+v", c.in, got, c.want)
		}
	}
}

func TestDecodeProbeErrors(t *testing.T) {
	bad := []string{
		"RC1 P AS5 12",             // missing i/n
		"RC1 P AS5 12 3/5 extra",   // extra field
		"RC1 P AS5 0 1/5",          // run 0
		"RC1 P AS5 10000 1/5",      // run too big
		"RC1 P AS5 012 1/5",        // leading zero run
		"RC1 P AS5 12 3",           // no slash
		"RC1 P AS5 12 0/5",         // index 0
		"RC1 P AS5 12 6/5",         // index > total
		"RC1 P AS5 12 1/21",        // total too big
		"RC1 P AS5 12 01/5",        // leading zero index
		"RC1 P AS5 12 1/05",        // leading zero total
		"RC1 P AS5 12 1/",          // empty total
		"RC1 Q AS5 12 1-3 -23",     // missing via
		"RC1 Q AS5 12 1-3 -23 - x", // extra
		"RC1 Q AS5 12 0 -23 -",     // index 0
		"RC1 Q AS5 12 21 -23 -",    // index > MaxProbes
		"RC1 Q AS5 12 3,1 -23 -",   // not ascending
		"RC1 Q AS5 12 1,2 -23 -",   // adjacent should be a range
		"RC1 Q AS5 12 1-1 -23 -",   // degenerate range
		"RC1 Q AS5 12 1-3 23 -",    // positive level
		"RC1 Q AS5 12 1-3 -0 -",    // negative zero
		"RC1 Q AS5 12 1-3 -023 -",  // leading zero level
		"RC1 Q AS5 12 1-3 -100 -",  // below MinLevel
		"RC1 Q AS5 12 1-3 x -",     // lowercase unknown
		"RC1 Q AS5 12 1-3 -23 bad!",
		"RC1 Q AS5 12 1-3 -23 TOOLONGCALL",
	}
	for _, s := range bad {
		if msg, err := Decode(s); !errors.Is(err, ErrDecode) || msg != nil {
			t.Errorf("Decode(%q) = %+v, %v; want ErrDecode", s, msg, err)
		}
	}
}

func TestProbeRoundTrip(t *testing.T) {
	for i := uint8(1); i <= MaxProbes; i++ {
		p := Probe{CP: "START", Run: MaxProbeRun, Index: i, Total: MaxProbes}
		text, err := EncodeProbe(p)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(text)
		if err != nil || !reflect.DeepEqual(got, &p) {
			t.Fatalf("round trip %q: %+v, %v", text, got, err)
		}
	}
}

func TestProbeReplyFitsDefaultLength(t *testing.T) {
	// Worst case: the longest heard list for MaxProbes=20 is pairs with
	// gaps (35 chars, the maximum over all 2^20 subsets), plus the
	// longest code, run, level and via. It must still fit the default
	// 67, so raising MaxProbes or the code/via length fails here.
	r := ProbeReply{
		CP: "CP1234", Run: MaxProbeRun,
		Heard: []uint8{1, 2, 4, 5, 7, 8, 10, 11, 13, 14, 16, 17, 19, 20},
		Level: MinLevel, Via: "ABCDEF-12",
	}
	text, err := EncodeProbeReply(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(text) > DefaultMaxTextLen {
		t.Fatalf("worst-case reply %q is %d chars, over %d", text, len(text), DefaultMaxTextLen)
	}
}

func TestHeardTokenWorstCaseIsExhaustive(t *testing.T) {
	if testing.Short() {
		t.Skip("exhaustive over 2^20 subsets")
	}
	longest := 0
	vals := make([]uint32, 0, MaxProbes)
	for mask := 1; mask < 1<<MaxProbes; mask++ {
		vals = vals[:0]
		for i := range MaxProbes {
			if mask&(1<<i) != 0 {
				vals = append(vals, uint32(i+1))
			}
		}
		longest = max(longest, len(seqListToken(vals)))
	}
	if longest != 35 {
		t.Fatalf("longest heard token = %d; update TestProbeReplyFitsDefaultLength's worst case", longest)
	}
}
