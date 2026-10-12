package uvk5

import (
	"context"
	"errors"
	"fmt"
)

// DirectSession drives a FakeRadio in-process: the same commands and
// replies as a Session, without a serial port or goroutines, so
// simulations can step it in simulated time.
type DirectSession struct {
	r      *FakeRadio
	ts     uint32
	events chan any
	done   chan struct{}
}

// directEvents bounds a step's monitor lines; a simulation steps often.
const directEvents = 4096

// NewDirectSession handshakes with r, checks its features and turns its
// monitor on.
func NewDirectSession(r *FakeRadio) (*DirectSession, error) {
	d := &DirectSession{r: r, ts: randomTS(), events: make(chan any, directEvents), done: make(chan struct{})}
	r.mu.Lock()
	r.sink = func(line string) {
		select {
		case d.events <- ParseLine(line):
		default:
		}
	}
	r.mu.Unlock()
	hs := make([]byte, 4)
	put32(hs, d.ts)
	if _, ok := r.handle(Frame{ID: cmdHandshake, Data: hs}); !ok {
		return nil, ErrNoReply
	}
	caps, ok := r.handle(Frame{ID: cmdCapabilities})
	if !ok {
		return nil, ErrFirmware
	}
	c, err := parseCapabilities(caps)
	if err != nil || !c.Has(Required) {
		return nil, ErrFirmware
	}
	r.handle(Frame{ID: cmdMonitor, Data: []byte{1}})
	return d, nil
}

func (d *DirectSession) Send(ctx context.Context, to, text string) (SendResult, error) {
	data, err := sendData(d.ts, to, text)
	if err != nil {
		return SendResult{}, err
	}
	reply, ok := d.r.handle(Frame{ID: cmdSendMessage, Data: data})
	if !ok {
		return SendResult{}, ErrNoReply
	}
	return checkSendReply(reply)
}

func (d *DirectSession) Events() <-chan any    { return d.events }
func (d *DirectSession) Done() <-chan struct{} { return d.done }
func (d *DirectSession) Err() error {
	select {
	case <-d.done:
		return ErrClosed
	default:
		return nil
	}
}

func (d *DirectSession) Settings() Settings {
	d.r.mu.Lock()
	defer d.r.mu.Unlock()
	return d.r.settings
}

func (d *DirectSession) Capabilities() Capabilities {
	d.r.mu.Lock()
	defer d.r.mu.Unlock()
	return d.r.caps
}

func (d *DirectSession) FirmwareVersion() string { return fakeVersion }

func (d *DirectSession) Status(ctx context.Context) (Status, error) {
	reply, ok := d.r.handle(Frame{ID: cmdStatus})
	if !ok {
		return Status{}, ErrUnsupported
	}
	return parseStatus(reply)
}

func (d *DirectSession) SetCallsign(ctx context.Context, cs string) error {
	call, ssid, err := SplitCallsign(cs)
	if err != nil {
		return err
	}
	reply, _ := d.r.handle(Frame{ID: cmdSetCallsign, Data: setCallsignData(d.ts, call, ssid)})
	if len(reply) < 4 || reply[0] != 1 {
		r := ReasonNone
		if len(reply) >= 4 {
			r = Reason(reply[3])
		}
		return &RefusedError{Cmd: "the callsign", Reason: r}
	}
	return nil
}

// Close ends the session (the radio is unplugged).
func (d *DirectSession) Close() error {
	select {
	case <-d.done:
		return errors.New("uvk5: already closed")
	default:
		close(d.done)
	}
	d.r.mu.Lock()
	d.r.sink = nil
	d.r.monitor = false
	d.r.mu.Unlock()
	return nil
}

// String names the session in logs.
func (d *DirectSession) String() string { return fmt.Sprintf("direct session to %s", d.r.Callsign()) }
