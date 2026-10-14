package epd

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"time"
)

// The Pervasive Displays G2 COG (V231_G2), as on PiSupply's PaPiRus
// HATs. Ported from repaper/gratis PlatformWithOS/driver-common/V231_G2/
// epd.c (Copyright 2013-2015 Pervasive Displays, Inc., Apache-2.0):
// power-up and COG checks, the four timed stages of an image change, and
// power-down. Each SPI transfer is one chip-select cycle, as gratis does.

// G2Pin is one of the panel's control lines.
type G2Pin int

const (
	PinPanelOn G2Pin = iota
	PinBorder
	PinDischarge
	PinReset
)

// G2Bus is the wire to a G2 panel.
type G2Bus interface {
	// Tx is one SPI transfer (chip select held for its length); r, if
	// not nil, receives what came back.
	Tx(w, r []byte) error
	SetPin(p G2Pin, high bool) error
	Busy() (bool, error)
	// Temperature is the panel's temperature (the PaPiRus LM75B); false
	// when unknown.
	Temperature() (int, bool)
	Sleep(d time.Duration)
	Now() time.Time
	Close() error
}

// G2 errors (gratis EPD_error).
var (
	ErrG2UnsupportedCOG = errors.New("epd: the panel's COG isn't a G2 (wrong panel or wiring?)")
	ErrG2PanelBroken    = errors.New("epd: the panel reports itself broken")
	ErrG2DCFailed       = errors.New("epd: the panel's DC/DC didn't start")
	ErrG2Busy           = errors.New("epd: the panel stayed busy")
)

const g2BusyTimeout = 5 * time.Second

type g2Stage int

const (
	stageCompensate g2Stage = iota // B -> W, W -> B (current image)
	stageWhite                     // B -> N, W -> W (current image)
	stageInverse                   // B -> N, W -> B (new image)
	stageNormal                    // B -> B, W -> W (new image)
)

// g2 drives one G2 panel.
type g2 struct {
	bus  G2Bus
	sz   g2Size
	name string
	last []byte // the frame on the panel (G2 layout), nil before the first
	took time.Duration
}

// NewG2 drives a PaPiRus panel of size ctrl (a PaPiRus name) over bus.
func NewG2(bus G2Bus, ctrl string) (Display, error) {
	sz, ok := g2Sizes[ctrl]
	if !ok {
		return nil, fmt.Errorf("epd: %q isn't a PaPiRus size", ctrl)
	}
	d := &g2{bus: bus, sz: sz, name: ctrl}
	d.powerOff()
	return d, nil
}

func (d *g2) bytesPerLine() int { return d.sz.dots / 8 }

// PackG2 converts a landscape image of the panel's size to the G2's
// frame: lines of dots/8 bytes, bit 0 the leftmost pixel, 1 = black.
func PackG2(img image.Image, ctrl string) []byte {
	sz := g2Sizes[ctrl]
	bpl := sz.dots / 8
	buf := make([]byte, sz.lines*bpl)
	b := img.Bounds()
	for y := range sz.lines {
		for x := range sz.dots {
			if x >= b.Dx() || y >= b.Dy() {
				continue
			}
			g := color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray)
			if g.Y < 128 {
				buf[y*bpl+x/8] |= 1 << (x % 8)
			}
		}
	}
	return buf
}

// Full draws img: the old image is washed out and the new one driven in
// over four stages (gratis EPD_image; EPD_clear first, the first time).
func (d *g2) Full(img image.Image) error {
	start := d.bus.Now()
	frame := PackG2(img, d.name)
	if err := d.begin(); err != nil {
		return err
	}
	if d.last == nil {
		d.fixedRepeat(0xff, stageCompensate) // clear: anything -> white
		d.fixedRepeat(0xff, stageWhite)
		d.fixedRepeat(0xaa, stageInverse)
		d.fixedRepeat(0xaa, stageNormal)
		d.fixedRepeat(0xaa, stageCompensate) // image_0
		d.fixedRepeat(0xaa, stageWhite)
	} else {
		d.dataRepeat(d.last, stageCompensate)
		d.dataRepeat(d.last, stageWhite)
	}
	d.dataRepeat(frame, stageInverse)
	d.dataRepeat(frame, stageNormal)
	d.end()
	d.last = frame
	d.took = d.bus.Now().Sub(start)
	return nil
}

// Partial isn't used: a G2 partial update ghosts; Full instead.
func (d *g2) Partial(img image.Image) error { return d.Full(img) }
func (d *g2) CanPartial() bool              { return false }
func (d *g2) Close() error                  { d.powerOff(); return d.bus.Close() }

func (d *g2) tx(w ...byte) { _ = d.bus.Tx(w, nil) }

func (d *g2) read(w ...byte) byte {
	r := make([]byte, len(w))
	_ = d.bus.Tx(w, r)
	return r[len(r)-1]
}

func (d *g2) pin(p G2Pin, high bool) { _ = d.bus.SetPin(p, high) }

// spiIdle sends a zero byte so MOSI and the clock rest low (gratis
// SPI_on / SPI_off).
func (d *g2) spiIdle() { d.tx(0x00) }

func (d *g2) reg(index, value byte) {
	d.tx(0x70, index)
	d.tx(0x72, value)
}

// begin powers the panel up and starts the COG (gratis EPD_begin).
func (d *g2) begin() error {
	d.pin(PinReset, false)
	d.pin(PinPanelOn, false)
	d.pin(PinDischarge, false)
	d.pin(PinBorder, false)
	d.spiIdle()
	d.bus.Sleep(5 * time.Millisecond)
	d.pin(PinPanelOn, true)
	d.bus.Sleep(10 * time.Millisecond)
	d.pin(PinReset, true)
	d.pin(PinBorder, true)
	d.bus.Sleep(5 * time.Millisecond)
	d.pin(PinReset, false)
	d.bus.Sleep(5 * time.Millisecond)
	d.pin(PinReset, true)
	d.bus.Sleep(5 * time.Millisecond)
	if err := d.waitIdle(); err != nil {
		d.powerOff()
		return err
	}
	d.read(0x71, 0x00)
	if id := d.read(0x71, 0x00); id&0x0f != 0x02 {
		d.powerOff()
		return fmt.Errorf("%w (COG id %#02x)", ErrG2UnsupportedCOG, id)
	}
	d.reg(0x02, 0x40) // disable OE
	d.tx(0x70, 0x0f)  // breakage
	if d.read(0x73, 0x00)&0x80 == 0 {
		d.powerOff()
		return ErrG2PanelBroken
	}
	d.reg(0x0b, 0x02) // power saving mode
	d.tx(0x70, 0x01)  // channel select
	d.tx(d.sz.channelSelect...)
	d.reg(0x07, 0xd1) // high power mode osc
	d.reg(0x08, 0x02) // power setting
	d.reg(0x09, 0xc2) // Vcom level
	d.reg(0x04, 0x03) // power setting
	d.reg(0x03, 0x01) // driver latch on
	d.reg(0x03, 0x00) // driver latch off
	d.bus.Sleep(5 * time.Millisecond)
	dcOK := false
	for range 4 {
		d.reg(0x05, 0x01) // charge pump positive voltage on
		d.bus.Sleep(240 * time.Millisecond)
		d.reg(0x05, 0x03) // negative
		d.bus.Sleep(40 * time.Millisecond)
		d.reg(0x05, 0x0f) // Vcom
		d.bus.Sleep(40 * time.Millisecond)
		d.tx(0x70, 0x0f)
		if d.read(0x73, 0x00)&0x40 != 0 {
			dcOK = true
			break
		}
	}
	if !dcOK {
		d.powerOff()
		return ErrG2DCFailed
	}
	d.reg(0x02, 0x04) // output enable to disable
	return nil
}

func (d *g2) waitIdle() error {
	deadline := d.bus.Now().Add(g2BusyTimeout)
	for {
		busy, err := d.bus.Busy()
		if err != nil {
			return err
		}
		if !busy {
			return nil
		}
		if d.bus.Now().After(deadline) {
			return ErrG2Busy
		}
		d.bus.Sleep(10 * time.Microsecond)
	}
}

// end finishes an update and powers the COG down (gratis EPD_end).
func (d *g2) end() {
	for range d.sz.lines { // nothing frame
		d.oneLine(0x7fff, nil, 0x00, stageCompensate)
	}
	if d.sz.pulseBorder {
		d.oneLine(0x7fff, nil, 0x00, stageCompensate) // dummy line
		d.bus.Sleep(25 * time.Millisecond)
		d.pin(PinBorder, false)
		d.bus.Sleep(200 * time.Millisecond)
		d.pin(PinBorder, true)
	} else {
		d.oneLine(0x7fff, nil, 0x00, stageNormal) // border dummy line
		d.bus.Sleep(200 * time.Millisecond)
	}
	d.reg(0x0b, 0x00)
	d.reg(0x03, 0x01) // latch reset on
	d.reg(0x05, 0x03) // charge pump Vcom off
	d.reg(0x05, 0x01) // negative off
	d.bus.Sleep(120 * time.Millisecond)
	d.reg(0x04, 0x80) // discharge internal
	d.reg(0x05, 0x00) // all charge pumps off
	d.reg(0x07, 0x01) // osc off
	d.bus.Sleep(50 * time.Millisecond)
	d.powerOff()
}

// powerOff drops power and signals and discharges the panel.
func (d *g2) powerOff() {
	d.pin(PinReset, false)
	d.pin(PinPanelOn, false)
	d.pin(PinBorder, false)
	d.spiIdle()
	d.pin(PinDischarge, true)
	d.bus.Sleep(150 * time.Millisecond)
	d.pin(PinDischarge, false)
}

// stageTime is one stage's length at the panel's temperature.
func (d *g2) stageTime() time.Duration {
	t, ok := d.bus.Temperature()
	if !ok {
		t = 25
	}
	return time.Duration(d.sz.stageMS*tempFactor10(t)/10) * time.Millisecond
}

// tempFactor10 is ten times the stage-time factor for a temperature.
func tempFactor10(c int) int {
	switch {
	case c <= -10:
		return 170
	case c <= -5:
		return 120
	case c <= 5:
		return 80
	case c <= 10:
		return 40
	case c <= 15:
		return 30
	case c <= 20:
		return 20
	case c <= 40:
		return 10
	}
	return 7
}

// fixedRepeat and dataRepeat send whole frames until the stage time is
// up (at least once).
func (d *g2) fixedRepeat(v byte, st g2Stage) {
	until := d.bus.Now().Add(d.stageTime())
	for {
		for l := range d.sz.lines {
			d.oneLine(l, nil, v, st)
		}
		if !d.bus.Now().Before(until) {
			return
		}
	}
}

func (d *g2) dataRepeat(frame []byte, st g2Stage) {
	until := d.bus.Now().Add(d.stageTime())
	bpl := d.bytesPerLine()
	for {
		for l := range d.sz.lines {
			d.oneLine(l, frame[l*bpl:(l+1)*bpl], 0, st)
		}
		if !d.bus.Now().Before(until) {
			return
		}
	}
}

// oneLine sends one line: its data bytes and scan byte (gratis
// one_line). data nil sends fixed for every data byte.
func (d *g2) oneLine(line int, data []byte, fixed byte, st g2Stage) {
	d.spiIdle()
	d.tx(0x70, 0x0a)
	buf := make([]byte, 0, 3+2*d.bytesPerLine()+2*d.sz.scanBytes)
	buf = append(buf, 0x72)
	if d.sz.preBorderByte {
		buf = append(buf, 0x00)
	}
	if d.sz.middleScan {
		buf = d.oddPixels(buf, data, fixed, st)
		for b := d.sz.scanBytes; b > 0; b-- {
			if line/4 == b-1 {
				buf = append(buf, 0x03<<(2*(line&0x03)))
			} else {
				buf = append(buf, 0x00)
			}
		}
		buf = d.evenPixels(buf, data, fixed, st)
	} else {
		for b := range d.sz.scanBytes {
			if line&0x01 != 0 && line/8 == b {
				buf = append(buf, 0xc0>>(line&0x06))
			} else {
				buf = append(buf, 0x00)
			}
		}
		buf = d.allPixels(buf, data, fixed, st)
		for b := d.sz.scanBytes; b > 0; b-- {
			if line&0x01 == 0 && line/8 == b-1 {
				buf = append(buf, 0x03<<(line&0x06))
			} else {
				buf = append(buf, 0x00)
			}
		}
	}
	switch d.sz.border {
	case borderZero:
		buf = append(buf, 0x00)
	case borderSet:
		if st == stageNormal {
			buf = append(buf, 0xaa)
		} else {
			buf = append(buf, 0x00)
		}
	}
	d.tx(buf...)
	d.reg(0x02, 0x07) // output data to panel
	d.spiIdle()
}

// evenPixels: display pixels are numbered from 1, so "even" are bits
// 1,3,5,7 of each byte, bytes in order.
func (d *g2) evenPixels(buf, data []byte, fixed byte, st g2Stage) []byte {
	for b := range d.bytesPerLine() {
		if data == nil {
			buf = append(buf, fixed)
			continue
		}
		px := data[b] & 0xaa
		switch st {
		case stageCompensate:
			px = 0xaa | ((px ^ 0xaa) >> 1)
		case stageWhite:
			px = 0x55 + ((px ^ 0xaa) >> 1)
		case stageInverse:
			px = 0x55 | (px ^ 0xaa)
		case stageNormal:
			px = 0xaa | (px >> 1)
		}
		p1, p2, p3, p4 := (px>>6)&0x03, (px>>4)&0x03, (px>>2)&0x03, px&0x03
		buf = append(buf, p1|p2<<2|p3<<4|p4<<6)
	}
	return buf
}

// oddPixels: bits 0,2,4,6, bytes in reverse.
func (d *g2) oddPixels(buf, data []byte, fixed byte, st g2Stage) []byte {
	for b := d.bytesPerLine(); b > 0; b-- {
		if data == nil {
			buf = append(buf, fixed)
			continue
		}
		px := data[b-1] & 0x55
		switch st {
		case stageCompensate:
			px = 0xaa | (px ^ 0x55)
		case stageWhite:
			px = 0x55 + (px ^ 0x55)
		case stageInverse:
			px = 0x55 | ((px ^ 0x55) << 1)
		case stageNormal:
			px = 0xaa | px
		}
		buf = append(buf, px)
	}
	return buf
}

// allPixels: both, interleaved, bytes in reverse (the 1.9" and 2.6").
func (d *g2) allPixels(buf, data []byte, fixed byte, st g2Stage) []byte {
	for b := d.bytesPerLine(); b > 0; b-- {
		if data == nil {
			buf = append(buf, fixed, fixed)
			continue
		}
		px := interleave(data[b-1])
		switch st {
		case stageCompensate:
			px = 0xaaaa | (px ^ 0x5555)
		case stageWhite:
			px = 0x5555 + (px ^ 0x5555)
		case stageInverse:
			px = 0x5555 | ((px ^ 0x5555) << 1)
		case stageNormal:
			px = 0xaaaa | px
		}
		buf = append(buf, byte(px>>8), byte(px))
	}
	return buf
}

// interleave spreads a byte's bits: 76543210 -> .7.6.5.4.3.2.1.0
func interleave(v byte) uint16 {
	x := uint16(v)
	x = (x | x<<4) & 0x0f0f
	x = (x | x<<2) & 0x3333
	x = (x | x<<1) & 0x5555
	return x
}
