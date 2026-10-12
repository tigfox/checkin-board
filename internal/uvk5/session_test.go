package uvk5

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type aired struct {
	from, to string
	m        Message
}

// connect runs a fake radio on one end of a pipe and a session on the other.
func connect(t *testing.T, edit func(*FakeRadio)) (*Session, *FakeRadio, *fakeClock, *[]aired) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)}
	radio := NewFakeRadio("KD2DCM-4", clk.Now)
	var mu sync.Mutex
	var air []aired
	radio.Air = func(from, to string, m Message) {
		mu.Lock()
		air = append(air, aired{from, to, m})
		mu.Unlock()
	}
	if edit != nil {
		edit(radio)
	}
	host, dev := net.Pipe()
	go func() { _ = radio.Serve(dev) }()
	t.Cleanup(func() { _ = dev.Close() })
	s, err := Connect(context.Background(), host)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, radio, clk, &air
}

func nextEvent(t *testing.T, s *Session) any {
	t.Helper()
	select {
	case e := <-s.Events():
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
		return nil
	}
}

func TestConnect(t *testing.T) {
	s, _, _, _ := connect(t, nil)
	if s.Version != fakeVersion || !s.Caps.Has(Required) || s.Caps.QueueDepth != 4 {
		t.Fatalf("session = %q %+v", s.Version, s.Caps)
	}
	if st := s.Settings(); st.Callsign != "KD2DCM-4" || !st.APRSOn || !st.Configured() {
		t.Fatalf("settings = %+v", st)
	}
}

func TestOldFirmwareRefused(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	for name, features := range map[string]uint32{"no capability command": 0, "no queue": FeatSendInfo | FeatSentDrop | FeatMultiAck} {
		radio := NewFakeRadio("KD2DCM-4", clk.Now)
		radio.SetFeatures(features)
		host, dev := net.Pipe()
		go func() { _ = radio.Serve(dev) }()
		_, err := Connect(context.Background(), host)
		if !errors.Is(err, ErrFirmware) {
			t.Errorf("%s: err = %v", name, err)
		}
		_ = dev.Close()
	}
}

func TestSendAndSent(t *testing.T) {
	s, radio, clk, air := connect(t, nil)
	ctx := context.Background()
	res, err := s.Send(ctx, "kd2dcm-1", "RC1 H CTL 0 130000")
	if err != nil || res.Line != "01" || res.FreeSlots != 3 {
		t.Fatalf("send: %+v %v", res, err)
	}
	radio.Tick()
	if e, ok := nextEvent(t, s).(Sent); !ok || e.To != "KD2DCM-1" || e.Line != "01" {
		t.Fatalf("event = %#v", e)
	}
	if len(*air) != 1 || (*air)[0].m.ID != "01" || (*air)[0].m.Text != "RC1 H CTL 0 130000" {
		t.Fatalf("air = %+v", *air)
	}
	// The gap: the next message waits 5 s.
	if _, err := s.Send(ctx, "KD2DCM-1", "second"); err != nil {
		t.Fatal(err)
	}
	radio.Tick()
	if len(*air) != 1 {
		t.Fatal("sent inside the 5 s gap")
	}
	clk.Add(5 * time.Second)
	radio.Tick()
	if len(*air) != 2 || (*air)[1].m.ID != "02" {
		t.Fatalf("air = %+v", *air)
	}
}

func TestQueueFullRefused(t *testing.T) {
	s, _, _, _ := connect(t, nil)
	ctx := context.Background()
	for i := range 4 {
		if _, err := s.Send(ctx, "KD2DCM-1", "m"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	_, err := s.Send(ctx, "KD2DCM-1", "fifth")
	var re *RefusedError
	if !errors.As(err, &re) || re.Reason != ReasonFull {
		t.Fatalf("err = %v", err)
	}
}

func TestSendValidation(t *testing.T) {
	s, _, _, _ := connect(t, nil)
	ctx := context.Background()
	long := make([]byte, 68)
	for i := range long {
		long[i] = 'x'
	}
	for name, tc := range map[string][2]string{
		"bad call": {"NOT A CALL", "hi"}, "empty": {"KD2DCM-1", ""}, "too long": {"KD2DCM-1", string(long)},
		"brace": {"KD2DCM-1", "a{b"}, "pipe": {"KD2DCM-1", "a|b"}, "tilde": {"KD2DCM-1", "a~b"}, "control": {"KD2DCM-1", "a\tb"},
	} {
		if _, err := s.Send(ctx, tc[0], tc[1]); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if err := ValidText(string(long[:67])); err != nil {
		t.Errorf("67 characters: %v", err)
	}
}

func TestHearAckAndRSSI(t *testing.T) {
	s, radio, _, air := connect(t, nil)
	radio.SetRSSI(-87)
	radio.Hear("KD2DCM-1", []string{"WIDE1-1"}, Message{Addressee: "KD2DCM-4", Text: "RC1 G CTL 3", ID: "050"})
	h, ok := nextEvent(t, s).(Heard)
	if !ok || h.Packet.Src != "KD2DCM-1" {
		t.Fatalf("heard = %#v", h)
	}
	if m, ok := ParseMessage(h.Packet.Info); !ok || m.ID != "050" || m.Text != "RC1 G CTL 3" {
		t.Fatalf("message = %+v", m)
	}
	if r, ok := nextEvent(t, s).(HeardRSSI); !ok || r.DBm != -87 {
		t.Fatalf("rssi = %#v", r)
	}
	// Auto-ack goes out first.
	radio.Tick()
	if len(*air) != 1 || !(*air)[0].m.Ack || (*air)[0].m.ID != "050" || (*air)[0].to != "KD2DCM-1" {
		t.Fatalf("air = %+v", *air)
	}
}

func TestDropOnBusyAndBattery(t *testing.T) {
	s, radio, clk, _ := connect(t, nil)
	ctx := context.Background()
	radio.SetBusy(true)
	if _, err := s.Send(ctx, "KD2DCM-1", "stuck"); err != nil {
		t.Fatal(err)
	}
	radio.Tick() // it reaches the front: its minute starts
	clk.Add(fakeGiveUp)
	radio.Tick()
	if d, ok := nextEvent(t, s).(Dropped); !ok || d.Reason != ReasonBusy || d.Line != "01" {
		t.Fatalf("drop = %#v", d)
	}
	radio.SetBusy(false)
	radio.SetLowBattery(true)
	if _, err := s.Send(ctx, "KD2DCM-1", "flat"); err != nil {
		t.Fatal(err)
	}
	radio.Tick()
	if d, ok := nextEvent(t, s).(Dropped); !ok || d.Reason != ReasonBattery {
		t.Fatalf("drop = %#v", d)
	}
}

func TestStatusAndSetCallsign(t *testing.T) {
	s, _, _, _ := connect(t, nil)
	ctx := context.Background()
	st, err := s.Status(ctx)
	if err != nil || st.FreqHz != 144_390_000 || !st.Monitor || st.Inhibit != ReasonNone {
		t.Fatalf("status = %+v %v", st, err)
	}
	if err := s.SetCallsign(ctx, "KD2DCM-9"); err != nil {
		t.Fatal(err)
	}
	if s.Settings().Callsign != "KD2DCM-9" {
		t.Fatalf("callsign = %q", s.Settings().Callsign)
	}
	if err := s.SetCallsign(ctx, "bad call"); err == nil {
		t.Fatal("bad callsign accepted")
	}
}

func TestUnsupportedFeatures(t *testing.T) {
	s, _, _, _ := connect(t, func(r *FakeRadio) { r.SetFeatures(Required) })
	if _, err := s.Status(context.Background()); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("status: %v", err)
	}
	if err := s.SetCallsign(context.Background(), "KD2DCM-9"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("set callsign: %v", err)
	}
}

func TestSessionEndsWhenPortCloses(t *testing.T) {
	clk := &fakeClock{t: time.Now()}
	radio := NewFakeRadio("KD2DCM-4", clk.Now)
	host, dev := net.Pipe()
	go func() { _ = radio.Serve(dev) }()
	s, err := Connect(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	_ = dev.Close() // unplugged
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("session didn't end")
	}
	if !errors.Is(s.Err(), ErrClosed) {
		t.Fatalf("err = %v", s.Err())
	}
	if _, err := s.Send(context.Background(), "KD2DCM-1", "x"); err == nil {
		t.Fatal("send on a closed session")
	}
}

func TestNoReply(t *testing.T) {
	host, dev := net.Pipe()
	go func() { // a port that swallows everything
		b := make([]byte, 256)
		for {
			if _, err := dev.Read(b); err != nil {
				return
			}
		}
	}()
	defer dev.Close()
	start := time.Now()
	_, err := Connect(context.Background(), host)
	if !errors.Is(err, ErrNoReply) || time.Since(start) > 2*replyTimeout {
		t.Fatalf("err = %v after %s", err, time.Since(start))
	}
}

// Spec 8.2: a fifth pending ack is dropped and reported, not the oldest.
func TestAckListFull(t *testing.T) {
	s, radio, _, _ := connect(t, nil)
	for i := range 5 {
		radio.Hear(fmt.Sprintf("KD2DCM-%d", i+1), nil, Message{Addressee: "KD2DCM-4", Text: "x", ID: "1"})
	}
	var drop Dropped
	for range 12 { // 5 frames x (APRSRAW + APRSRSSI) + the DROP
		if d, ok := nextEvent(t, s).(Dropped); ok {
			drop = d
			break
		}
	}
	if !drop.Ack || drop.To != "KD2DCM-5" || drop.Reason != ReasonFull {
		t.Fatalf("drop = %+v", drop)
	}
}
