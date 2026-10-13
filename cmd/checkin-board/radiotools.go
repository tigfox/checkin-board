package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"checkin-board/internal/firmware"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/uvk5"
	"checkin-board/internal/web"
)

// radioTools are a UV-K5 node's extras: UV Studio's serial bridge and
// the firmware slot writer. Both borrow the radio's port from the backend.
type radioTools struct {
	rb   *radiobackend.Backend
	port string       // CB_UVK5_PORT ("": found)
	last atomic.Value // string: the port the backend last connected on
	log  *slog.Logger
}

// radioPort is where the radio is: the configured port, the one the
// backend last used, or a port with the radio's USB vendor (never just
// any serial device: raising DTR resets an Arduino, keys an AIOC).
func (t *radioTools) radioPort() (string, error) {
	if t.port != "" {
		return t.port, nil
	}
	if p, _ := t.last.Load().(string); p != "" {
		return p, nil
	}
	if radios, _ := uvk5.RadioPorts(uvk5.Candidates()); len(radios) > 0 {
		return radios[0], nil
	}
	return "", errors.New("no UV-K5 found: is it plugged in by USB-C and switched on?")
}

// Open lends the radio to UV Studio and opens its port with DTR up (the
// firmware sends its screen only then).
func (t *radioTools) Open(ctx context.Context) (web.BridgePort, error) {
	giveBack, err := t.rb.Lend(ctx, "UV Studio")
	if err != nil {
		return nil, err
	}
	p, err := t.openDTR()
	if err != nil {
		giveBack()
		return nil, err
	}
	return &lentPort{ReadWriteCloser: p, name: p.name, giveBack: giveBack}, nil
}

type namedPort struct {
	dtrPort
	name string
}

func (t *radioTools) openDTR() (namedPort, error) {
	path, err := t.radioPort()
	if err != nil {
		return namedPort{}, err
	}
	p, err := openRadioPort(path)
	if err != nil {
		return namedPort{}, err
	}
	if err := p.SetDTR(true); err != nil {
		_ = p.Close()
		return namedPort{}, fmt.Errorf("%s: raise DTR: %w", path, err)
	}
	return namedPort{p, path}, nil
}

// openRadioPort opens path for UV Studio's bridge, refusing an AIOC (it
// keys PTT from DTR).
func openRadioPort(path string) (dtrPort, error) {
	if err := uvk5.SafeForDTR(path); err != nil {
		return nil, err
	}
	rw, err := uvk5.OpenPort(path)
	if err != nil {
		return nil, err
	}
	p, ok := rw.(dtrPort)
	if !ok {
		_ = rw.Close()
		return nil, fmt.Errorf("%s: can't set DTR on this port", path)
	}
	return p, nil
}

// dtrPort is an open serial port whose DTR can be raised.
type dtrPort interface {
	io.ReadWriteCloser
	SetDTR(on bool) error
}

// lentPort gives the radio back when UV Studio closes it.
type lentPort struct {
	io.ReadWriteCloser
	name     string
	giveBack func()
	once     sync.Once
}

func (l *lentPort) Name() string { return l.name }

func (l *lentPort) Close() error {
	err := l.ReadWriteCloser.Close()
	l.once.Do(l.giveBack)
	return err
}

// newSlots writes the radio's firmware slots from the node's folder.
func (t *radioTools) newSlots(dir string) *firmware.Slots {
	return firmware.NewSlots(firmware.SlotsConfig{
		Folder:  firmware.NewFolder(dir),
		Lend:    t.rb.Lend,
		Connect: t.connectBasic,
		Logger:  t.log,
	})
}

// connectBasic opens a handshake-only session on the radio's port (the
// slot commands need nothing more, whatever firmware the radio runs).
func (t *radioTools) connectBasic(ctx context.Context) (firmware.SlotSession, error) {
	path, err := t.radioPort()
	if err != nil {
		return nil, err
	}
	if err := uvk5.SafeForDTR(path); err != nil { // opening pulses DTR/RTS
		return nil, err
	}
	rw, err := uvk5.OpenPort(path)
	if err != nil {
		return nil, err
	}
	return uvk5.ConnectBasic(ctx, rw)
}
