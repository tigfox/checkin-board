// Package papirus is PiSupply's PaPiRus HAT hardware: a Pervasive
// Displays G2 e-paper panel on SPI with its control lines, and the HAT's
// LM75B temperature sensor on I2C (periph.io, pure Go). It only runs on a
// Raspberry Pi, so it is tested on the bench (checkin-board panel -test),
// not in unit tests; the G2 protocol is tested in package epd.
package papirus

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"periph.io/x/conn/v3"
	"periph.io/x/conn/v3/gpio"
	"periph.io/x/conn/v3/gpio/gpioreg"
	"periph.io/x/conn/v3/i2c"
	"periph.io/x/conn/v3/i2c/i2creg"
	"periph.io/x/conn/v3/physic"
	"periph.io/x/conn/v3/spi"
	"periph.io/x/conn/v3/spi/spireg"
	"periph.io/x/host/v3"

	"checkin-board/internal/panel/epd"
)

// The PaPiRus wiring (Raspberry Pi GPIO numbers; gratis epd_io.h, which
// PiSupply's papirus-setup uses).
const (
	spiPort      = "SPI0.0"
	pinPanelOn   = "GPIO23"
	pinBorder    = "GPIO14"
	pinDischarge = "GPIO15"
	pinReset     = "GPIO24"
	pinBusy      = "GPIO25"
	spiSpeed     = 8 * physic.MegaHertz
	lm75bAddr    = 0x48
	// hatProduct is where the HAT's EEPROM name shows.
	hatProduct = "/proc/device-tree/hat/product"
)

// Detect reports whether a PaPiRus HAT is fitted (its EEPROM names it).
func Detect() bool {
	b, err := os.ReadFile(hatProduct)
	return err == nil && strings.Contains(strings.ToLower(string(b)), "papirus")
}

var (
	hostOnce sync.Once
	hostErr  error
)

// Open drives the PaPiRus panel of size ctrl (epd.PaPiRus27, ...).
func Open(ctrl string) (epd.Display, error) {
	if !epd.IsPaPiRus(ctrl) {
		return nil, fmt.Errorf("papirus: %q isn't a PaPiRus size", ctrl)
	}
	b, err := openBus()
	if err != nil {
		return nil, err
	}
	d, err := epd.NewG2(b, ctrl)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return d, nil
}

type bus struct {
	port spi.PortCloser
	conn conn.Conn
	pins [4]gpio.PinIO // by epd.G2Pin
	busy gpio.PinIO
	i2c  i2c.BusCloser // nil: no temperature
}

func openBus() (*bus, error) {
	hostOnce.Do(func() { _, hostErr = host.Init() })
	if hostErr != nil {
		return nil, fmt.Errorf("papirus: init GPIO/SPI: %w", hostErr)
	}
	port, err := spireg.Open(spiPort)
	if err != nil {
		return nil, fmt.Errorf("papirus: open %s (is SPI enabled? raspi-config nonint do_spi 0): %w", spiPort, err)
	}
	c, err := port.Connect(spiSpeed, spi.Mode0, 8)
	if err != nil {
		port.Close()
		return nil, fmt.Errorf("papirus: SPI: %w", err)
	}
	b := &bus{port: port, conn: c}
	for i, name := range []string{pinPanelOn, pinBorder, pinDischarge, pinReset} {
		p := gpioreg.ByName(name)
		if p == nil {
			port.Close()
			return nil, fmt.Errorf("papirus: no %s (is this a Raspberry Pi?)", name)
		}
		if err := p.Out(gpio.Low); err != nil {
			port.Close()
			return nil, fmt.Errorf("papirus: %s: %w", name, err)
		}
		b.pins[i] = p
	}
	if b.busy = gpioreg.ByName(pinBusy); b.busy == nil {
		port.Close()
		return nil, fmt.Errorf("papirus: no %s", pinBusy)
	}
	if err := b.busy.In(gpio.PullNoChange, gpio.NoEdge); err != nil {
		port.Close()
		return nil, fmt.Errorf("papirus: %s: %w", pinBusy, err)
	}
	if ib, err := i2creg.Open(""); err == nil { // the LM75B; optional
		b.i2c = ib
	}
	return b, nil
}

func (b *bus) Tx(w, r []byte) error { return b.conn.Tx(w, r) }

func (b *bus) SetPin(p epd.G2Pin, high bool) error {
	l := gpio.Low
	if high {
		l = gpio.High
	}
	return b.pins[p].Out(l)
}

func (b *bus) Busy() (bool, error) { return b.busy.Read() == gpio.High, nil }

// Temperature reads the HAT's LM75B (whole degrees C).
func (b *bus) Temperature() (int, bool) {
	if b.i2c == nil {
		return 0, false
	}
	var r [2]byte
	if err := (&i2c.Dev{Addr: lm75bAddr, Bus: b.i2c}).Tx([]byte{0x00}, r[:]); err != nil {
		return 0, false
	}
	return int(int8(r[0])), true
}

func (b *bus) Sleep(d time.Duration) { time.Sleep(d) }
func (b *bus) Now() time.Time        { return time.Now() }

func (b *bus) Close() error {
	var errs []error
	if b.i2c != nil {
		errs = append(errs, b.i2c.Close())
	}
	return errors.Join(append(errs, b.port.Close())...)
}
