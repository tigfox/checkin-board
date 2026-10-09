package epd

import (
	"fmt"
	"image"
	"image/color"
	"time"
)

// Controller command sequences for the Adafruit 2.13" bonnet revisions.
// They follow Adafruit's MIT-licensed CircuitPython EPD driver
// (adafruit_epd: ssd1680.py, ssd1680b.py, ssd1675.py; SPDX MIT,
// Copyright Adafruit Industries), with partial refresh after GxEPD2's
// for the same SSD1680 panels.

// Native panel geometry: the controllers address the panel in portrait,
// 122 pixels across (padded to 128 bits) by 250 rows.
const (
	nativeW    = Height // 122
	nativeH    = Width  // 250
	rowBytes   = (nativeW + 7) / 8
	frameBytes = rowBytes * nativeH
)

// Timeouts for the BUSY line.
const (
	resetTimeout   = 2 * time.Second
	refreshTimeout = 15 * time.Second
)

// Commands shared by the SSD1675 and SSD1680.
const (
	cmdDriverControl = 0x01
	cmdGateVoltage   = 0x03
	cmdSourceVoltage = 0x04
	cmdDeepSleep     = 0x10
	cmdDataMode      = 0x11
	cmdSWReset       = 0x12
	cmdActivate      = 0x20
	cmdUpdateCtrl2   = 0x22
	cmdWriteBW       = 0x24
	cmdWriteRed      = 0x26
	cmdVCOM          = 0x2C
	cmdWriteLUT      = 0x32
	cmdDummyLine     = 0x3A
	cmdGateLine      = 0x3B
	cmdBorder        = 0x3C
	cmdRAMXPos       = 0x44
	cmdRAMYPos       = 0x45
	cmdRAMXCount     = 0x4E
	cmdRAMYCount     = 0x4F
	cmdAnalogBlock   = 0x74
	cmdDigitalBlock  = 0x7E
)

// ssd1675LUT is the SSD1675 waveform (70 bytes) and its voltage, dummy
// line and gate line settings (bytes 70-75).
var ssd1675LUT = []byte{
	0x80, 0x60, 0x40, 0x00, 0x00, 0x00, 0x00, 0x10, 0x60, 0x20, 0x00, 0x00, 0x00, 0x00, 0x80, 0x60,
	0x40, 0x00, 0x00, 0x00, 0x00, 0x10, 0x60, 0x20, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x03, 0x03, 0x00, 0x00, 0x02, 0x09, 0x09, 0x00, 0x00, 0x02, 0x03, 0x03, 0x00,
	0x00, 0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x15, 0x41, 0xA8, 0x32, 0x30, 0x0A,
}

// Bus is the wire to a controller: SPI with the data/command line, the
// reset line and the BUSY input.
type Bus interface {
	Reset() error
	Command(cmd byte, data ...byte) error
	Data(p []byte) error
	WaitIdle(timeout time.Duration) error
	Close() error
}

// Pack converts a landscape image (Width×Height; dark pixels black) to
// the controller's native frame: portrait rows of rowBytes, MSB first,
// 1 = white.
func Pack(img image.Image) []byte {
	buf := make([]byte, frameBytes)
	for i := range buf {
		buf[i] = 0xFF
	}
	b := img.Bounds()
	for y := range Height {
		for x := range Width {
			g := color.GrayModel.Convert(img.At(b.Min.X+x, b.Min.Y+y)).(color.Gray)
			if g.Y >= 128 {
				continue
			}
			nx, ny := nativeW-1-y, x
			buf[ny*rowBytes+nx/8] &^= 0x80 >> (nx % 8)
		}
	}
	return buf
}

// panel drives one controller over a Bus.
type panel struct {
	bus     Bus
	ctrl    string
	last    []byte // the frame on the panel now (for partial refresh)
	variant int    // partial refresh sequence (bench: SetPartialVariant)
	took    time.Duration
}

// NewPanel drives controller ctrl over bus.
func NewPanel(bus Bus, ctrl string) (Display, error) {
	if !Known(ctrl) {
		return nil, fmt.Errorf("epd: unknown controller %q", ctrl)
	}
	return &panel{bus: bus, ctrl: ctrl, variant: defaultPartial}, nil
}

// seq runs commands, stopping at the first error.
type seq struct {
	bus Bus
	err error
}

func (s *seq) cmd(c byte, data ...byte) {
	if s.err == nil {
		s.err = s.bus.Command(c, data...)
	}
}

func (s *seq) data(p []byte) {
	if s.err == nil {
		s.err = s.bus.Data(p)
	}
}

func (s *seq) wait(timeout time.Duration) {
	if s.err == nil {
		s.err = s.bus.WaitIdle(timeout)
	}
}

func (s *seq) reset() {
	if s.err == nil {
		s.err = s.bus.Reset()
	}
}

// powerUp wakes the controller (from deep sleep, by hardware reset) and
// configures it.
func (p *panel) powerUp(s *seq) {
	s.reset()
	s.wait(resetTimeout)
	s.cmd(cmdSWReset)
	s.wait(resetTimeout)
	switch p.ctrl {
	case SSD1680Z:
		s.cmd(cmdDriverControl, byte(nativeH), byte(nativeH>>8), 0x00)
		s.cmd(cmdDataMode, 0x03)
		s.cmd(cmdVCOM, 0x36)
		s.cmd(cmdGateVoltage, 0x17)
		s.cmd(cmdSourceVoltage, 0x41, 0x00, 0x32)
		s.cmd(cmdRAMXPos, 0x00, byte(nativeW/8))
		s.cmd(cmdRAMYPos, 0x00, 0x00, byte(nativeH), byte(nativeH>>8))
		s.cmd(cmdBorder, 0x05)
	case SSD1680:
		s.cmd(cmdDriverControl, byte(nativeH-1), byte((nativeH-1)>>8), 0x00)
		s.cmd(cmdDataMode, 0x03)
		s.cmd(cmdVCOM, 0x36)
		s.cmd(cmdGateVoltage, 0x17)
		s.cmd(cmdSourceVoltage, 0x41, 0x00, 0x32)
		s.cmd(cmdRAMXPos, 0x00, byte(rowBytes-1))
		s.cmd(cmdRAMYPos, 0x00, 0x00, byte(nativeH-1), byte((nativeH-1)>>8))
		s.cmd(cmdBorder, 0x05)
	case SSD1675:
		s.cmd(cmdAnalogBlock, 0x54)
		s.cmd(cmdDigitalBlock, 0x3B)
		s.cmd(cmdDriverControl, 0xFA, 0x01, 0x00)
		s.cmd(cmdDataMode, 0x03)
		s.cmd(cmdRAMXPos, 0x00, 0x0F)
		s.cmd(cmdRAMYPos, 0x00, 0x00, 0xF9, 0x00)
		s.cmd(cmdBorder, 0x03)
		s.cmd(cmdVCOM, 0x70)
		s.cmd(cmdGateVoltage, ssd1675LUT[70])
		s.cmd(cmdSourceVoltage, ssd1675LUT[71:74]...)
		s.cmd(cmdDummyLine, ssd1675LUT[74])
		s.cmd(cmdGateLine, ssd1675LUT[75])
		s.cmd(cmdWriteLUT, ssd1675LUT[:70]...)
	}
	s.wait(resetTimeout)
}

// writeRAM sends a frame to one RAM, from address 0,0.
func (p *panel) writeRAM(s *seq, cmd byte, frame []byte) {
	s.cmd(cmdRAMXCount, 0x00)
	s.cmd(cmdRAMYCount, 0x00, 0x00)
	s.cmd(cmd)
	s.data(frame)
}

// refresh triggers the update, waits for it (timing the waveform), and
// puts the controller to deep sleep (it draws nothing and holds the
// image).
func (p *panel) refresh(s *seq, mode byte) {
	s.cmd(cmdUpdateCtrl2, mode)
	s.cmd(cmdActivate)
	start := time.Now()
	s.wait(refreshTimeout)
	p.took = time.Since(start)
	s.cmd(cmdDeepSleep, 0x01)
}

// Partial refresh sequences (SSD1680 parts). The panel's own waveform
// for display mode 2 drives only the pixels that differ between the red
// RAM (previous frame) and the BW RAM (new frame). Variants differ in
// the setup and update flags; the bench (checkin-board panel
// -test-partial) times and shows each.
const (
	defaultPartial = 1
	maxPartial     = 4
	cmdUpdateCtrl1 = 0x21
	cmdTempSensor  = 0x18
)

// SetPartialVariant selects the partial refresh sequence (bench only).
func SetPartialVariant(d Display, v int) bool {
	p, ok := d.(*panel)
	if !ok || p == nil || v < 1 || v > maxPartial {
		return false
	}
	p.variant = v
	return true
}

// UpdateTook is how long the controller was busy in the last update's
// waveform (a mode that drives nothing returns at once).
func UpdateTook(d Display) time.Duration {
	if p, ok := d.(*panel); ok {
		return p.took
	}
	return -1
}

// Full redraws the panel.
func (p *panel) Full(img image.Image) error {
	frame := Pack(img)
	s := &seq{bus: p.bus}
	p.powerUp(s)
	p.writeRAM(s, cmdWriteBW, frame)
	mode := byte(0xF4)
	switch p.ctrl {
	case SSD1675:
		p.writeRAM(s, cmdWriteRed, frame) // both RAMs hold the image
	default:
		p.writeRAM(s, cmdWriteRed, make([]byte, frameBytes))
		if p.ctrl == SSD1680Z {
			mode = 0xF7
		}
	}
	p.refresh(s, mode)
	if s.err != nil {
		p.last = nil // unknown: the next refresh must be full
		return fmt.Errorf("epd %s: full refresh: %w", p.ctrl, s.err)
	}
	p.last = frame
	return nil
}

// Partial updates only what changed (SSD1680 parts): the controller
// compares the previous frame (red RAM) with the new one (BW RAM).
func (p *panel) Partial(img image.Image) error {
	if !p.CanPartial() {
		return fmt.Errorf("epd %s: no partial refresh", p.ctrl)
	}
	if p.last == nil {
		// Nothing of ours on the panel yet (just opened, or the last
		// refresh failed): there's no previous frame to compare with.
		return p.Full(img)
	}
	frame := Pack(img)
	s := &seq{bus: p.bus}
	p.powerUp(s)
	if p.variant >= 2 {
		// The internal temperature sensor and normal RAM options, as
		// GxEPD2 and Waveshare set them for these panels.
		s.cmd(cmdUpdateCtrl1, 0x00, 0x80)
		s.cmd(cmdTempSensor, 0x80)
	}
	s.cmd(cmdBorder, 0x80)
	if p.variant != 4 {
		p.writeRAM(s, cmdWriteRed, p.last)
	}
	p.writeRAM(s, cmdWriteBW, frame)
	mode := byte(0xFC)
	if p.variant >= 3 {
		mode = 0xFF
	}
	p.refresh(s, mode)
	if s.err != nil {
		p.last = nil
		return fmt.Errorf("epd %s: partial refresh: %w", p.ctrl, s.err)
	}
	p.last = frame
	return nil
}

// CanPartial reports whether the controller has a partial refresh.
func (p *panel) CanPartial() bool { return p.ctrl != SSD1675 }

// Close releases the bus.
func (p *panel) Close() error { return p.bus.Close() }
