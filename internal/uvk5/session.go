package uvk5

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	// replyTimeout bounds a command's wait for its reply.
	replyTimeout = 3 * time.Second
	eventBuffer  = 512
)

var (
	// ErrFirmware is a radio whose firmware lacks what checkin-board
	// needs (requests 1-4): it must be reflashed.
	ErrFirmware = errors.New("uvk5: firmware too old for checkin-board: reflash it with the current APRS firmware")
	// ErrNoReply is a command the radio didn't answer in time.
	ErrNoReply = errors.New("uvk5: the radio didn't answer")
	// ErrClosed is a session whose port is gone.
	ErrClosed = errors.New("uvk5: session closed")
	// ErrUnsupported is a command this firmware doesn't have.
	ErrUnsupported = errors.New("uvk5: not supported by this firmware")
)

// RefusedError is a command the radio answered with Ok = 0.
type RefusedError struct {
	Cmd    string
	Reason Reason
}

func (e *RefusedError) Error() string {
	if e.Reason == ReasonNone {
		return fmt.Sprintf("uvk5: radio refused %s", e.Cmd)
	}
	return fmt.Sprintf("uvk5: radio refused %s: %s", e.Cmd, e.Reason)
}

// Session is one handshaken connection to a radio. Safe for concurrent
// use; commands are sent one at a time.
type Session struct {
	rw io.ReadWriteCloser

	Version  string
	Caps     Capabilities
	ts       uint32
	settings Settings

	wmu     sync.Mutex
	cmdMu   sync.Mutex
	smu     sync.Mutex
	replies chan Frame
	events  chan any
	done    chan struct{}
	errOnce sync.Once
	err     error
}

// DTRSetter is a port whose DTR line can be set (a real serial port).
type DTRSetter interface {
	SetDTR(on bool) error
}

// Connect handshakes on rw, checks the firmware has what checkin-board
// needs, reads the settings and turns the monitor on. It takes rw over:
// Close closes it.
func Connect(ctx context.Context, rw io.ReadWriteCloser) (*Session, error) {
	s := &Session{rw: rw, replies: make(chan Frame, 8), events: make(chan any, eventBuffer), done: make(chan struct{})}
	go s.read()
	if err := s.setup(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) setup(ctx context.Context) error {
	s.ts = randomTS()
	data := make([]byte, 4)
	put32(data, s.ts)
	f, err := s.command(ctx, cmdHandshake, data)
	if err != nil {
		return fmt.Errorf("uvk5: handshake: %w", err)
	}
	if len(f.Data) >= 16 {
		s.Version = cString(f.Data[:16])
	}
	f, err = s.command(ctx, cmdCapabilities, nil)
	if errors.Is(err, ErrNoReply) {
		return fmt.Errorf("%w (firmware %q has no capability command)", ErrFirmware, s.Version)
	}
	if err != nil {
		return fmt.Errorf("uvk5: capabilities: %w", err)
	}
	if s.Caps, err = parseCapabilities(f.Data); err != nil {
		return err
	}
	if !s.Caps.Has(Required) {
		return fmt.Errorf("%w (firmware %q, features %#x)", ErrFirmware, s.Version, s.Caps.Features)
	}
	if _, err := s.ReadSettings(ctx); err != nil {
		return err
	}
	// Over USB-C the firmware writes monitor lines (APRSRAW, SENT:,
	// DROP:) only while DTR is up: it takes DTR as "a terminal is open"
	// (cdc_acm_data_send_with_dtr). Command replies don't need it. It is
	// raised only now, when the port has answered as a UV-K5, never on a
	// port that might be an AIOC (which keys PTT from DTR). Bench,
	// 2026-10-13: with DTR low no line ever arrived.
	if d, ok := s.rw.(DTRSetter); ok {
		if err := d.SetDTR(true); err != nil {
			return fmt.Errorf("uvk5: raise DTR: %w", err)
		}
	}
	f, err = s.command(ctx, cmdMonitor, []byte{1})
	if err != nil {
		return fmt.Errorf("uvk5: monitor on: %w", err)
	}
	if len(f.Data) < 1 || f.Data[0] != 1 {
		return &RefusedError{Cmd: "monitor on"}
	}
	return nil
}

func randomTS() uint32 {
	var b [4]byte
	for {
		_, _ = rand.Read(b[:])
		if v := binary.LittleEndian.Uint32(b[:]); v != 0 {
			return v
		}
	}
}

// read splits the stream: replies to the waiting command, monitor lines
// to Events.
func (s *Session) read() {
	r := NewReader(s.rw)
	for {
		it, err := r.Next()
		if errors.Is(err, ErrFrame) {
			continue
		}
		if err != nil {
			s.fail(err)
			return
		}
		if it.Frame != nil {
			select {
			case s.replies <- *it.Frame:
			default: // nobody waiting for this many: drop the oldest
				<-s.replies
				s.replies <- *it.Frame
			}
			continue
		}
		select {
		case s.events <- ParseLine(it.Line):
		case <-s.done:
			return
		}
	}
}

func (s *Session) fail(err error) {
	s.errOnce.Do(func() {
		if errors.Is(err, io.EOF) {
			err = ErrClosed
		}
		s.err = err
		close(s.done)
	})
}

// Done is closed when the session ends (port gone, Close).
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended.
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Events delivers monitor lines as Heard, HeardRSSI, Sent, Dropped or
// OtherLine values, in order.
func (s *Session) Events() <-chan any { return s.events }

// closeQuiet bounds Close's "monitor off".
const closeQuiet = 500 * time.Millisecond

// Close turns monitoring off (so the next program on the port, such as
// UV Studio, doesn't get APRS lines in its stream), ends the session and
// closes the port.
func (s *Session) Close() error {
	if s.Err() == nil {
		ctx, cancel := context.WithTimeout(context.Background(), closeQuiet)
		_, _ = s.command(ctx, cmdMonitor, []byte{0})
		cancel()
	}
	s.fail(ErrClosed)
	return s.rw.Close()
}

// command sends a command and waits for its reply (id + 1).
func (s *Session) command(ctx context.Context, id uint16, data []byte) (Frame, error) {
	s.cmdMu.Lock()
	defer s.cmdMu.Unlock()
	for len(s.replies) > 0 { // stale replies from timed-out commands
		<-s.replies
	}
	s.wmu.Lock()
	_, err := s.rw.Write(EncodeFrame(id, data, false))
	s.wmu.Unlock()
	if err != nil {
		s.fail(err)
		return Frame{}, err
	}
	timer := time.NewTimer(replyTimeout)
	defer timer.Stop()
	for {
		select {
		case f := <-s.replies:
			if f.ID == id+1 {
				return f, nil
			}
		case <-timer.C:
			return Frame{}, ErrNoReply
		case <-s.done:
			return Frame{}, ErrClosed
		case <-ctx.Done():
			return Frame{}, ctx.Err()
		}
	}
}

func (s *Session) withTS(rest []byte) []byte {
	d := make([]byte, 4+len(rest))
	put32(d, s.ts)
	copy(d[4:], rest)
	return d
}

// ReadSettings reads the radio's settings record (callsign, APRS on).
func (s *Session) ReadSettings(ctx context.Context) (Settings, error) {
	f, err := s.command(ctx, cmdGetSettings, nil)
	if err != nil {
		return Settings{}, fmt.Errorf("uvk5: settings: %w", err)
	}
	st, err := parseSettings(f.Data)
	if err != nil {
		return Settings{}, err
	}
	s.smu.Lock()
	s.settings = st
	s.smu.Unlock()
	return st, nil
}

// Settings is the last settings read.
func (s *Session) Settings() Settings {
	s.smu.Lock()
	defer s.smu.Unlock()
	return s.settings
}

// Send queues a message: to is a callsign-SSID, text 1-67 printable
// ASCII characters without { | ~. The result carries the line number
// the radio will use ("07"); a full queue is refused (RefusedError with
// ReasonFull), never replacing anything.
func (s *Session) Send(ctx context.Context, to, text string) (SendResult, error) {
	data, err := sendData(s.ts, to, text)
	if err != nil {
		return SendResult{}, err
	}
	f, err := s.command(ctx, cmdSendMessage, data)
	if err != nil {
		return SendResult{}, fmt.Errorf("uvk5: send: %w", err)
	}
	return checkSendReply(f.Data)
}

// sendData builds 0x0700's data: ts, a 10-byte destination, the text.
func sendData(ts uint32, to, text string) ([]byte, error) {
	call, ssid, err := SplitCallsign(to)
	if err != nil {
		return nil, err
	}
	if err := ValidText(text); err != nil {
		return nil, err
	}
	d := make([]byte, 14, 14+len(text))
	put32(d, ts)
	copy(d[4:], callsign(call, ssid))
	return append(d, text...), nil
}

func checkSendReply(d []byte) (SendResult, error) {
	res, err := parseSendResult(d)
	if err != nil {
		return SendResult{}, err
	}
	if !res.OK {
		return res, &RefusedError{Cmd: "the message", Reason: res.Reason}
	}
	if res.Line == "" {
		return res, fmt.Errorf("uvk5: send reply has no line number")
	}
	return res, nil
}

// ValidText checks a message text for the radio: 1-67 printable ASCII
// characters, none of APRS's reserved { | ~.
func ValidText(text string) error {
	if text == "" || len(text) > MaxText {
		return fmt.Errorf("uvk5: message text must be 1-%d characters, got %d", MaxText, len(text))
	}
	for _, c := range []byte(text) {
		if c < 0x20 || c > 0x7E || c == '{' || c == '|' || c == '~' {
			return fmt.Errorf("uvk5: message text has a character the radio can't send (%q)", c)
		}
	}
	return nil
}

// Status reads battery, frequency, transmit inhibit and frame counters
// (requests 7 and 11).
func (s *Session) Status(ctx context.Context) (Status, error) {
	if !s.Caps.Has(FeatStatus) {
		return Status{}, ErrUnsupported
	}
	f, err := s.command(ctx, cmdStatus, nil)
	if err != nil {
		return Status{}, fmt.Errorf("uvk5: status: %w", err)
	}
	return parseStatus(f.Data)
}

// SetCallsign sets the radio's callsign-SSID (request 8).
func (s *Session) SetCallsign(ctx context.Context, cs string) error {
	if !s.Caps.Has(FeatSetCall) {
		return ErrUnsupported
	}
	call, ssid, err := SplitCallsign(cs)
	if err != nil {
		return err
	}
	f, err := s.command(ctx, cmdSetCallsign, setCallsignData(s.ts, call, ssid))
	if err != nil {
		return fmt.Errorf("uvk5: set callsign: %w", err)
	}
	if len(f.Data) < 1 || f.Data[0] != 1 {
		r := ReasonNone
		if len(f.Data) >= 4 {
			r = Reason(f.Data[3])
		}
		return &RefusedError{Cmd: "the callsign", Reason: r}
	}
	_, err = s.ReadSettings(ctx)
	return err
}

// Capabilities is what the radio's firmware can do.
func (s *Session) Capabilities() Capabilities { return s.Caps }

// FirmwareVersion is the firmware's version string from the handshake.
func (s *Session) FirmwareVersion() string { return s.Version }
