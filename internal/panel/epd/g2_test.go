package epd

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"testing"
	"time"
)

// fakeG2 is a G2 panel on a virtual clock: it records every transfer
// and pin change.
type fakeG2 struct {
	now     time.Time
	txs     [][]byte
	pins    []string
	cogID   byte
	broken  bool
	dcAfter int // DC/DC check reads before it says OK (-1: never)
	dcReads int
	busy    bool
	temp    int
	tempOK  bool
	lastReg byte
	closed  bool
}

func newFakeG2() *fakeG2 {
	return &fakeG2{now: time.Date(2026, 10, 13, 22, 0, 0, 0, time.UTC), cogID: 0x12, dcAfter: 0}
}

func (f *fakeG2) Tx(w, r []byte) error {
	f.now = f.now.Add(50 * time.Microsecond)
	f.txs = append(f.txs, append([]byte(nil), w...))
	if len(w) == 2 && w[0] == 0x70 {
		f.lastReg = w[1]
	}
	if r != nil {
		switch {
		case w[0] == 0x71:
			r[len(r)-1] = f.cogID
		case w[0] == 0x73 && f.lastReg == 0x0f && f.dcReads == 0 && !f.broken && f.pins != nil:
			// the first 0x0f read is the breakage check
			f.dcReads = -1
			r[len(r)-1] = 0x80
		case w[0] == 0x73 && f.lastReg == 0x0f:
			if f.dcReads == -1 {
				f.dcReads = 0
			}
			f.dcReads++
			if f.broken {
				r[len(r)-1] = 0x00
			} else if f.dcAfter >= 0 && f.dcReads > f.dcAfter {
				r[len(r)-1] = 0xc0
			} else {
				r[len(r)-1] = 0x80
			}
		}
	}
	return nil
}

func (f *fakeG2) SetPin(p G2Pin, high bool) error {
	name := []string{"panel_on", "border", "discharge", "reset"}[p]
	v := "0"
	if high {
		v = "1"
	}
	f.pins = append(f.pins, name+"="+v)
	return nil
}

func (f *fakeG2) Busy() (bool, error)      { f.now = f.now.Add(time.Millisecond); return f.busy, nil }
func (f *fakeG2) Temperature() (int, bool) { return f.temp, f.tempOK }
func (f *fakeG2) Sleep(d time.Duration)    { f.now = f.now.Add(d) }
func (f *fakeG2) Now() time.Time           { return f.now }
func (f *fakeG2) Close() error             { f.closed = true; return nil }

// lines returns the data-line transfers (those starting 0x72 that follow
// a 0x70 0x0a).
func (f *fakeG2) lines() [][]byte {
	var out [][]byte
	for i := 1; i < len(f.txs); i++ {
		if bytes.Equal(f.txs[i-1], []byte{0x70, 0x0a}) {
			out = append(out, f.txs[i])
		}
	}
	return out
}

func imageWith(size image.Point, black ...image.Point) *image.Gray {
	img := image.NewGray(image.Rectangle{Max: size})
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	for _, p := range black {
		img.SetGray(p.X, p.Y, color.Gray{})
	}
	return img
}

func TestPackG2(t *testing.T) {
	img := imageWith(Size(PaPiRus27), image.Pt(0, 0), image.Pt(9, 0), image.Pt(263, 175))
	f := PackG2(img, PaPiRus27)
	if len(f) != 176*33 || f[0] != 0x01 || f[1] != 0x02 || f[176*33-1] != 0x80 {
		t.Fatalf("frame: %d bytes, %#02x %#02x %#02x", len(f), f[0], f[1], f[len(f)-1])
	}
}

func TestG2Full27(t *testing.T) {
	bus := newFakeG2()
	d, err := NewG2(bus, PaPiRus27)
	if err != nil {
		t.Fatal(err)
	}
	if Size(PaPiRus27) != image.Pt(264, 176) || !IsPaPiRus(PaPiRus27) || !Known(PaPiRus27) {
		t.Fatal("size or name")
	}
	start := bus.now
	if err := d.Full(imageWith(Size(PaPiRus27), image.Pt(0, 0))); err != nil {
		t.Fatal(err)
	}
	// Power-up: reset/panel/discharge/border low, then panel on, reset
	// pulses, and finally discharge after power-off.
	want := []string{"reset=0", "panel_on=0", "border=0", "discharge=1", "discharge=0", // NewG2's power-off
		"reset=0", "panel_on=0", "discharge=0", "border=0", "panel_on=1", "reset=1", "border=1", "reset=0", "reset=1"}
	for i, p := range want {
		if bus.pins[i] != p {
			t.Fatalf("pin %d = %s, want %s (all %v)", i, bus.pins[i], p, bus.pins[:len(want)])
		}
	}
	lines := bus.lines()
	// Six fixed stages (clear, image_0's first two) and two data stages,
	// each at least one frame of 176 lines, then the nothing frame and
	// the dummy line.
	if n := len(lines); n < 8*176+177 || (n-177)%176 != 0 {
		t.Fatalf("%d lines", n)
	}
	for _, l := range lines {
		if len(l) != 1+1+33+44+33 {
			t.Fatalf("line length %d", len(l))
		}
	}
	// The last image line before the nothing frame: line 0 of the final
	// (normal) stage, with the leftmost pixel black.
	end := len(lines) - 177
	l0 := lines[end-176]
	exp := []byte{0x72, 0x00}
	exp = append(exp, bytes.Repeat([]byte{0xaa}, 32)...)
	exp = append(exp, 0xab)
	exp = append(exp, make([]byte, 43)...)
	exp = append(exp, 0x03)
	exp = append(exp, bytes.Repeat([]byte{0xaa}, 33)...)
	if !bytes.Equal(l0, exp) {
		t.Fatalf("line 0:\n got % x\nwant % x", l0, exp)
	}
	// The 2.7" pulses the border at the end, then powers off.
	tail := bus.pins[len(bus.pins)-7:]
	if tail[0] != "border=0" || tail[1] != "border=1" || tail[len(tail)-1] != "discharge=0" {
		t.Fatalf("end pins %v", tail)
	}
	// Stage time 630 ms at 25 C: eight stages take at least 5 s.
	if took := bus.now.Sub(start); took < 8*630*time.Millisecond {
		t.Fatalf("took %v", took)
	}
	// A second image washes out the first instead of clearing: four
	// timed stages, not eight.
	second := bus.now
	if err := d.Full(imageWith(Size(PaPiRus27))); err != nil {
		t.Fatal(err)
	}
	if took := bus.now.Sub(second); took < 4*630*time.Millisecond || took >= 6*630*time.Millisecond {
		t.Fatalf("second image took %v, want four stages", took)
	}
	if d.CanPartial() {
		t.Fatal("partial on")
	}
	_ = d.Close()
	if !bus.closed {
		t.Fatal("bus not closed")
	}
}

func TestG2Errors(t *testing.T) {
	for name, c := range map[string]struct {
		edit func(*fakeG2)
		want error
	}{
		"wrong COG": {func(f *fakeG2) { f.cogID = 0x11 }, ErrG2UnsupportedCOG},
		"broken":    {func(f *fakeG2) { f.broken = true }, ErrG2PanelBroken},
		"no DC/DC":  {func(f *fakeG2) { f.dcAfter = -1 }, ErrG2DCFailed},
		"busy":      {func(f *fakeG2) { f.busy = true }, ErrG2Busy},
	} {
		bus := newFakeG2()
		c.edit(bus)
		d, _ := NewG2(bus, PaPiRus27)
		err := d.Full(imageWith(Size(PaPiRus27)))
		if !errors.Is(err, c.want) {
			t.Errorf("%s: %v", name, err)
		}
		if last := bus.pins[len(bus.pins)-1]; last != "discharge=0" {
			t.Errorf("%s: not powered off (%s)", name, last)
		}
	}
	// The DC/DC comes up on the third try.
	bus := newFakeG2()
	bus.dcAfter = 2
	d, _ := NewG2(bus, PaPiRus27)
	if err := d.Full(imageWith(Size(PaPiRus27))); err != nil {
		t.Fatalf("third try: %v", err)
	}
}

func TestG2TemperatureAndSizes(t *testing.T) {
	cold := newFakeG2()
	cold.temp, cold.tempOK = 5, true
	d, _ := NewG2(cold, PaPiRus27)
	start := cold.now
	_ = d.Full(imageWith(Size(PaPiRus27)))
	if took := cold.now.Sub(start); took < 8*630*8*time.Millisecond/10 {
		t.Fatalf("cold took %v, want 8 stages of 5.04 s", took)
	}
	// The 2.6": scan-data-scan lines with a border byte.
	bus := newFakeG2()
	d, _ = NewG2(bus, PaPiRus26)
	if err := d.Full(imageWith(Size(PaPiRus26))); err != nil {
		t.Fatal(err)
	}
	for _, l := range bus.lines() {
		if len(l) != 1+16+2*29+16+1 {
			t.Fatalf("2.6 line length %d", len(l))
		}
	}
	if Size(PaPiRus26) != image.Pt(232, 128) || Size(SSD1680Z) != image.Pt(Width, Height) {
		t.Fatal("sizes")
	}
}
