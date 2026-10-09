// Package bonnet is the Adafruit 2.13" e-ink bonnet's hardware: its
// SPI bus and control lines for the display, and its two buttons
// (periph.io, pure Go). It only runs on a Raspberry Pi, so it is tested
// on the bench (checkin-board panel -test), not in unit tests.
package bonnet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"periph.io/x/conn/v3"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"

	"checkin-board/internal/panel/epd"
)

// The Adafruit 2.13" e-ink bonnet's wiring (Raspberry Pi GPIO numbers).
const (
	spiPort  = "SPI0.0" // CE0
	pinDC    = "GPIO22"
	pinReset = "GPIO27"
	pinBusy  = "GPIO17"
	pinTop   = "GPIO5"
	pinBot   = "GPIO6"
	spiSpeed = 4 * physic.MegaHertz
	// chunk keeps each SPI transfer under spidev's default buffer.
	chunk = 1024
)

var (
	hostOnce sync.Once
	hostErr  error
)

func initHost() error {
	hostOnce.Do(func() { _, hostErr = host.Init() })
	return hostErr
}

// bonnetBus is the bonnet's controller over periph.io.
type bonnetBus struct {
	port          spi.PortCloser
	conn          conn.Conn
	dc, rst, busy gpio.PinIO
}

func pin(name string) (gpio.PinIO, error) {
	p := gpioreg.ByName(name)
	if p == nil {
		return nil, fmt.Errorf("bonnet: no %s (is this a Raspberry Pi?)", name)
	}
	return p, nil
}

func openBonnet() (*bonnetBus, error) {
	if err := initHost(); err != nil {
		return nil, fmt.Errorf("bonnet: init GPIO/SPI: %w", err)
	}
	port, err := spireg.Open(spiPort)
	if err != nil {
		return nil, fmt.Errorf("bonnet: open %s (is SPI enabled? raspi-config nonint do_spi 0): %w", spiPort, err)
	}
	c, err := port.Connect(spiSpeed, spi.Mode0, 8)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("bonnet: SPI: %w", err)
	}
	b := &bonnetBus{port: port, conn: c}
	for _, p := range []struct {
		name string
		dst  *gpio.PinIO
	}{{pinDC, &b.dc}, {pinReset, &b.rst}, {pinBusy, &b.busy}} {
		if *p.dst, err = pin(p.name); err != nil {
			port.Close()
			return nil, err
		}
	}
	if err := errors.Join(b.dc.Out(gpio.Low), b.rst.Out(gpio.High), b.busy.In(gpio.PullNoChange, gpio.NoEdge)); err != nil {
		port.Close()
		return nil, fmt.Errorf("bonnet: set up pins: %w", err)
	}
	return b, nil
}

func (b *bonnetBus) Reset() error {
	if err := b.rst.Out(gpio.Low); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	if err := b.rst.Out(gpio.High); err != nil {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (b *bonnetBus) Command(cmd byte, data ...byte) error {
	if err := b.dc.Out(gpio.Low); err != nil {
		return err
	}
	if err := b.conn.Tx([]byte{cmd}, nil); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return b.Data(data)
}

func (b *bonnetBus) Data(p []byte) error {
	if err := b.dc.Out(gpio.High); err != nil {
		return err
	}
	for len(p) > 0 {
		n := min(len(p), chunk)
		if err := b.conn.Tx(p[:n], nil); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// WaitIdle waits for BUSY to go low.
func (b *bonnetBus) WaitIdle(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for b.busy.Read() == gpio.High {
		if time.Now().After(deadline) {
			return fmt.Errorf("busy for over %s (wrong controller, or the panel isn't connected)", timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func (b *bonnetBus) Close() error { return b.port.Close() }

// Open opens the bonnet's display with controller ctrl.
func Open(ctrl string) (epd.Display, error) {
	if !epd.Known(ctrl) {
		return nil, fmt.Errorf("bonnet: unknown controller %q", ctrl)
	}
	b, err := openBonnet()
	if err != nil {
		return nil, err
	}
	return epd.NewPanel(b, ctrl)
}

// Button is a press of one of the bonnet's buttons.
type Button int

// The buttons.
const (
	Top Button = iota
	Bottom
)

// debounce ignores contact bounce after a press.
const debounce = 150 * time.Millisecond

// Buttons reports presses of the bonnet's two buttons (active low, with
// the bonnet's pull-ups) until ctx ends.
func Buttons(ctx context.Context, out chan<- Button) error {
	if err := initHost(); err != nil {
		return fmt.Errorf("bonnet: init GPIO: %w", err)
	}
	var wg sync.WaitGroup
	for _, bt := range []struct {
		name string
		b    Button
	}{{pinTop, Top}, {pinBot, Bottom}} {
		p, err := pin(bt.name)
		if err != nil {
			return err
		}
		if err := p.In(gpio.PullUp, gpio.FallingEdge); err != nil {
			return fmt.Errorf("bonnet: button %s: %w", bt.name, err)
		}
		wg.Go(func() {
			var last time.Time
			for ctx.Err() == nil {
				if !p.WaitForEdge(500 * time.Millisecond) {
					continue
				}
				if now := time.Now(); now.Sub(last) >= debounce && p.Read() == gpio.Low {
					last = now
					select {
					case out <- bt.b:
					case <-ctx.Done():
					}
				}
			}
		})
	}
	wg.Wait()
	return ctx.Err()
}
