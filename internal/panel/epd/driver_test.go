package epd

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"
)

// recBus records what a controller sequence sends.
type recBus struct {
	log     []string
	data    map[byte][]byte // last data written after each command
	cur     byte
	busyErr error
	resets  int
	waited  []time.Duration
}

func newRec() *recBus { return &recBus{data: map[byte][]byte{}} }

func (b *recBus) Reset() error { b.resets++; b.log = append(b.log, "reset"); return nil }
func (b *recBus) Command(cmd byte, data ...byte) error {
	b.cur = cmd
	b.data[cmd] = append([]byte(nil), data...)
	if len(data) <= 8 {
		b.log = append(b.log, fmt.Sprintf("%02X % X", cmd, data))
	} else {
		b.log = append(b.log, fmt.Sprintf("%02X [%d bytes]", cmd, len(data)))
	}
	return nil
}
func (b *recBus) Data(p []byte) error {
	b.data[b.cur] = append(b.data[b.cur], p...)
	return nil
}
func (b *recBus) WaitIdle(timeout time.Duration) error {
	b.waited = append(b.waited, timeout)
	b.log = append(b.log, "wait")
	return b.busyErr
}
func (b *recBus) Close() error { return nil }

func white() *image.Gray {
	img := image.NewGray(image.Rect(0, 0, Width, Height))
	for i := range img.Pix {
		img.Pix[i] = 255
	}
	return img
}

func TestPackMapsLandscapeToNativePortrait(t *testing.T) {
	img := white()
	img.SetGray(0, 0, color.Gray{})              // top-left
	img.SetGray(Width-1, Height-1, color.Gray{}) // bottom-right
	buf := Pack(img)
	if len(buf) != frameBytes {
		t.Fatalf("frame = %d bytes, want %d", len(buf), frameBytes)
	}
	// Landscape (x, y) is native (nx = 121 - y, ny = x); 1 bits are white.
	black := func(nx, ny int) bool { return buf[ny*rowBytes+nx/8]&(0x80>>(nx%8)) == 0 }
	if !black(nativeW-1, 0) || !black(0, nativeH-1) {
		t.Fatal("corners not mapped")
	}
	n := 0
	for i, by := range buf {
		for bit := range 8 {
			if by&(0x80>>bit) == 0 {
				n++
				_ = i
			}
		}
	}
	if n != 2 {
		t.Fatalf("%d black pixels, want 2 (padding columns must stay white)", n)
	}
}

func TestFullRefreshSequences(t *testing.T) {
	for _, c := range []struct {
		ctrl      string
		driverCtl string
		update    byte
		redFrame  bool // SSD1675 writes the image to both RAMs
	}{
		{SSD1680Z, "01 FA 00 00", 0xF7, false},
		{SSD1680, "01 F9 00 00", 0xF4, false},
		{SSD1675, "01 FA 01 00", 0xF4, true},
	} {
		b := newRec()
		d, err := NewPanel(b, c.ctrl)
		if err != nil {
			t.Fatal(err)
		}
		img := white()
		img.SetGray(10, 10, color.Gray{})
		if err := d.Full(img); err != nil {
			t.Fatal(err)
		}
		seq := strings.Join(b.log, "|")
		if !strings.HasPrefix(seq, "reset|wait|12 |wait") || !strings.Contains(seq, c.driverCtl) {
			t.Errorf("%s: sequence %s", c.ctrl, seq)
		}
		if got := b.data[0x22]; len(got) != 1 || got[0] != c.update {
			t.Errorf("%s: update mode % X, want %02X", c.ctrl, got, c.update)
		}
		if !bytes.Equal(b.data[0x24], Pack(img)) {
			t.Errorf("%s: BW RAM differs from the packed frame", c.ctrl)
		}
		red := b.data[0x26]
		if c.redFrame && !bytes.Equal(red, Pack(img)) || !c.redFrame && !bytes.Equal(red, make([]byte, frameBytes)) {
			t.Errorf("%s: red RAM wrong", c.ctrl)
		}
		if !strings.HasSuffix(seq, "10 01") {
			t.Errorf("%s: not put to deep sleep after the refresh: %s", c.ctrl, seq)
		}
	}
}

func TestPartialRefresh(t *testing.T) {
	b := newRec()
	d, _ := NewPanel(b, SSD1680Z)
	first := white()
	_ = d.Full(first)
	next := white()
	next.SetGray(5, 5, color.Gray{})
	if err := d.Partial(next); err != nil {
		t.Fatal(err)
	}
	// The controller compares the previous frame (red RAM) with the new.
	if !bytes.Equal(b.data[0x26], Pack(first)) || !bytes.Equal(b.data[0x24], Pack(next)) || b.data[0x22][0] != 0xFC {
		t.Fatalf("partial: red=%v new=%v mode % X", bytes.Equal(b.data[0x26], Pack(first)), bytes.Equal(b.data[0x24], Pack(next)), b.data[0x22])
	}
	old, _ := NewPanel(newRec(), SSD1675)
	if old.CanPartial() || old.Partial(next) == nil {
		t.Fatal("SSD1675 has no partial refresh")
	}
	// Partial needs a previous frame: the first refresh must be full.
	fresh, _ := NewPanel(newRec(), SSD1680)
	if err := fresh.Partial(next); err == nil {
		t.Fatal("partial refresh with nothing on the panel")
	}
}

func TestBusyTimeoutIsAnError(t *testing.T) {
	b := newRec()
	b.busyErr = errors.New("busy timeout")
	d, _ := NewPanel(b, SSD1680Z)
	if err := d.Full(white()); err == nil {
		t.Fatal("busy timeout swallowed")
	}
	if _, err := NewPanel(b, "il0373"); err == nil {
		t.Fatal("unknown controller accepted")
	}
}
