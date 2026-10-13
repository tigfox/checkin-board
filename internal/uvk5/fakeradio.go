package uvk5

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"
)

// FakeRadio behaves like the agreed APRS firmware for tests: the serial
// protocol on one side, a simulated channel on the other. Its limits
// are the firmware's: a queue of QueueDepth messages that refuses when
// full, line numbers 01-99, SENT/DROP lines, AckDepth pending acks sent
// before messages, a minimum Gap between its own transmissions, and a
// 60 s give-up when the channel stays busy. Time comes from Now, and
// Tick sends what's due (call it often, or run Run).
type FakeRadio struct {
	// Air puts a frame this radio transmits on the channel.
	Air func(from, to string, m Message)
	Now func() time.Time

	mu        sync.Mutex
	settings  Settings
	caps      Capabilities
	status    Status
	gap       time.Duration
	busy      bool // the channel never clears
	lowBatt   bool
	out       io.Writer
	sink      func(line string) // DirectSession: lines without a port
	wmu       sync.Mutex
	ts        uint32
	mb        *fakeSlots // firmware slots (fakeslots.go)
	noSlots   bool
	monitor   bool
	queue     []fakeQueued
	acks      []fakeAck
	line      int
	lastTX    time.Time
	headSince time.Time // when the front item started waiting on a busy channel
	rssi      int
	commands  []uint16
}

type fakeQueued struct {
	to, text, line string
	at             time.Time
}

type fakeAck struct {
	to, id string
	at     time.Time
}

const (
	fakeGiveUp  = 60 * time.Second
	fakeVersion = "f4hwn.aprs.2"
)

// NewFakeRadio returns a radio with callsign cs, APRS on, every agreed
// feature, a 4-deep queue and 4 pending acks.
func NewFakeRadio(cs string, now func() time.Time) *FakeRadio {
	call, ssid, err := SplitCallsign(cs)
	if err != nil {
		panic(err)
	}
	all := uint32(0x3ff) // revision 1: bits 0-9
	return &FakeRadio{
		Now:      now,
		settings: Settings{APRSOn: true, Call: call, SSID: ssid, Callsign: callsign(call, ssid)},
		caps:     Capabilities{Revision: 1, Features: all, QueueDepth: 4, AckDepth: 4, MaxText: MaxText},
		status:   Status{InBand: true, BatteryMV: 7900, FreqHz: 144_390_000},
		gap:      5 * time.Second,
		rssi:     -90,
	}
}

// Callsign is the radio's callsign-SSID.
func (f *FakeRadio) Callsign() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.settings.Callsign
}

// SetFeatures replaces the capability bits (an older firmware: 0).
func (f *FakeRadio) SetFeatures(features uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.caps.Features = features
}

// SetBusy makes the channel never clear (true) or clear again.
func (f *FakeRadio) SetBusy(busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy = busy
}

// SetLowBattery stops transmissions as the firmware does on a low battery.
func (f *FakeRadio) SetLowBattery(low bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lowBatt = low
}

// SetRSSI sets the signal strength reported for frames heard.
func (f *FakeRadio) SetRSSI(dbm int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rssi = dbm
}

// Queued is how many messages wait to go on air.
func (f *FakeRadio) Queued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queue)
}

// Serve answers host commands on rw until it closes. Monitor lines and
// replies are written to rw.
func (f *FakeRadio) Serve(rw io.ReadWriter) error {
	f.mu.Lock()
	f.out = rw
	f.mu.Unlock()
	r := NewReader(rw)
	for {
		it, err := r.Next()
		if errors.Is(err, ErrFrame) {
			continue
		}
		if err != nil {
			return err
		}
		if it.Frame == nil || !it.Frame.CRCOK {
			continue // the firmware ignores text and bad CRCs
		}
		if reply, ok := f.handle(*it.Frame); ok {
			f.write(EncodeFrame(it.Frame.ID+1, reply, true))
		}
	}
}

func (f *FakeRadio) write(b []byte) {
	f.mu.Lock()
	w := f.out
	f.mu.Unlock()
	if w == nil {
		return
	}
	f.wmu.Lock()
	defer f.wmu.Unlock()
	_, _ = w.Write(b)
}

// writeLine writes a monitor line if a host turned monitoring on.
func (f *FakeRadio) writeLine(on bool, s string) {
	if !on {
		return
	}
	f.mu.Lock()
	sink := f.sink
	f.mu.Unlock()
	if sink != nil {
		sink(s)
		return
	}
	f.write([]byte(s + "\r\n"))
}

func ack(ok bool) []byte { return []byte{b2u(ok), 0, 0, 0} }

func (f *FakeRadio) handle(c Frame) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, c.ID)
	tsOK := len(c.Data) >= 4 && f.ts != 0 && le32(c.Data) == f.ts
	switch c.ID {
	case cmdSlotInfo, cmdSlotErase, cmdSlotWrite, cmdSlotValidate:
		return f.handleSlot(c)
	case cmdHandshake:
		if len(c.Data) < 4 || le32(c.Data) == 0 {
			return nil, false
		}
		f.ts = le32(c.Data)
		v := make([]byte, 16)
		copy(v, fakeVersion)
		return v, true
	case cmdCapabilities:
		if f.caps.Features == 0 {
			return nil, false // an older firmware: no such command
		}
		return f.caps.encode(), true
	case cmdGetSettings:
		rec := make([]byte, 4+settingsRecordLen)
		rec[0] = 1
		copy(rec[4:], "APR1")
		rec[4+5] = b2u(f.settings.APRSOn)
		rec[4+7] = f.settings.SSID
		copy(rec[4+12:4+20], f.settings.Call)
		return rec, true
	case cmdMonitor:
		f.monitor = len(c.Data) > 0 && c.Data[0] == 1
		f.status.Monitor = f.monitor
		return ack(true), true
	case cmdSendMessage:
		return f.queueMessage(c.Data, tsOK), true
	case cmdStatus:
		if !f.caps.Has(FeatStatus) {
			return nil, false
		}
		st := f.status
		st.Busy, st.Monitor, st.Inhibit = f.busy, f.monitor, f.inhibit()
		st.WouldTransmit = st.Inhibit == ReasonNone && !f.busy
		st.FreeSlots, st.AcksWaiting = f.free(), uint8(len(f.acks))
		return st.encode(), true
	case cmdSetCallsign:
		refuse := func(r Reason) []byte { return []byte{0, 0, 0, uint8(r)} }
		switch {
		case !f.caps.Has(FeatSetCall):
			return nil, false
		case !tsOK:
			return refuse(ReasonSession), true
		case len(c.Data) != 13 || c.Data[10] != 0 || c.Data[11] != 0 || c.Data[12] > 15:
			return refuse(ReasonInvalid), true
		}
		call := cString(c.Data[4:12])
		if _, _, err := SplitCallsign(call); err != nil || strings.ToUpper(call) != call {
			return refuse(ReasonInvalid), true
		}
		f.settings.Call, f.settings.SSID = call, c.Data[12]
		f.settings.Callsign = callsign(call, c.Data[12])
		return ack(true), true
	}
	return nil, false
}

func (f *FakeRadio) queueMessage(d []byte, tsOK bool) []byte {
	refuse := func(r Reason) []byte { return []byte{0, 0, f.free(), uint8(r)} }
	if !tsOK {
		return refuse(ReasonSession)
	}
	if len(d) < 15 {
		return refuse(ReasonEmpty)
	}
	to := cString(d[4:14])
	text := cString(d[14:])
	switch {
	case to == "" || text == "":
		return refuse(ReasonEmpty)
	case len(text) > MaxText:
		return refuse(ReasonTooLong)
	}
	if len(f.queue) >= int(f.caps.QueueDepth) {
		return refuse(ReasonFull)
	}
	f.line = f.line%99 + 1
	line := fmt.Sprintf("%02d", f.line)
	f.queue = append(f.queue, fakeQueued{to: strings.ToUpper(to), text: text, line: line, at: f.Now()})
	return []byte{1, uint8(f.line), f.free(), 0}
}

func (f *FakeRadio) free() uint8 { return uint8(int(f.caps.QueueDepth) - len(f.queue)) }

// inhibit is why the radio won't transmit now. Caller holds f.mu.
func (f *FakeRadio) inhibit() Reason {
	switch {
	case !f.settings.Configured():
		return ReasonNoCall
	case !f.settings.APRSOn:
		return ReasonAPRSOff
	case !f.status.InBand:
		return ReasonBand
	case f.lowBatt:
		return ReasonBattery
	}
	return ReasonNone
}

// Tick drops what has waited too long or can't be sent, then transmits
// one item if the gap allows: a pending ack first, then a message.
func (f *FakeRadio) Tick() {
	f.mu.Lock()
	now := f.Now()
	var lines []string
	if r := f.inhibit(); r != ReasonNone {
		for _, q := range f.queue {
			lines = append(lines, fmt.Sprintf("DROP:%s{%s} %s", q.to, q.line, r))
		}
		f.queue = nil
	}
	// Spec 8.3: the item at the front gets 60 s of busy channel, then
	// it's given up and the next one gets its own minute (acks first).
	if f.busy && (len(f.acks) > 0 || len(f.queue) > 0) {
		if f.headSince.IsZero() {
			f.headSince = now
		}
		if now.Sub(f.headSince) >= fakeGiveUp {
			if len(f.acks) > 0 {
				a := f.acks[0]
				f.acks = f.acks[1:]
				lines = append(lines, fmt.Sprintf("DROP:ack %s{%s} %s", a.to, a.id, ReasonBusy))
			} else {
				q := f.queue[0]
				f.queue = f.queue[1:]
				lines = append(lines, fmt.Sprintf("DROP:%s{%s} %s", q.to, q.line, ReasonBusy))
			}
			f.headSince = now
		}
	} else if !f.busy {
		f.headSince = time.Time{}
	}
	var air func(from, to string, m Message)
	var from, to string
	var msg Message
	canTX := !f.busy && (f.lastTX.IsZero() || now.Sub(f.lastTX) >= f.gap) && f.inhibit() == ReasonNone
	switch {
	case canTX && len(f.acks) > 0:
		a := f.acks[0]
		f.acks = f.acks[1:]
		from, to, msg = f.settings.Callsign, a.to, Message{Addressee: a.to, ID: a.id, Ack: true}
	case canTX && len(f.queue) > 0:
		q := f.queue[0]
		f.queue = f.queue[1:]
		from, to, msg = f.settings.Callsign, q.to, Message{Addressee: q.to, Text: q.text, ID: q.line}
		lines = append(lines, fmt.Sprintf("SENT:%s{%s}", q.to, q.line))
		f.status.Sent++
	}
	if to != "" {
		f.lastTX, f.headSince = now, time.Time{}
		air = f.Air
	}
	monitor := f.monitor
	f.mu.Unlock()
	if air != nil {
		air(from, to, msg)
	}
	for _, l := range lines {
		f.writeLine(monitor, l)
	}
}

// Run calls Tick every interval until stop is closed.
func (f *FakeRadio) Run(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			f.Tick()
		}
	}
}

// Hear is a frame from the channel. It's printed to the monitor (with
// its signal strength); a message to this radio is acked automatically.
func (f *FakeRadio) Hear(from string, path []string, m Message) {
	f.mu.Lock()
	f.status.Decoded++
	own := strings.EqualFold(m.Addressee, f.settings.Callsign)
	var dropAck string
	if own && !m.Ack && !m.Rej && m.ID != "" {
		to := strings.ToUpper(from)
		dup := slices.ContainsFunc(f.acks, func(a fakeAck) bool { return a.to == to && a.id == m.ID })
		switch {
		case dup:
		case len(f.acks) >= int(f.caps.AckDepth): // spec 8.2: a fifth is dropped and reported
			dropAck = fmt.Sprintf("DROP:ack %s{%s} %s", to, m.ID, ReasonFull)
		default:
			f.acks = append(f.acks, fakeAck{to: to, id: m.ID, at: f.Now()})
		}
	}
	monitor, rssi, haveRSSI := f.monitor, f.rssi, f.caps.Has(FeatRSSI)
	f.mu.Unlock()
	raw, err := EncodeAX25(Packet{Dest: "APZCB1", Src: from, Path: path, Info: FormatMessage(m)})
	if err != nil {
		return
	}
	f.writeLine(monitor, FormatHeard(raw))
	if haveRSSI {
		f.writeLine(monitor, fmt.Sprintf("APRSRSSI:%d", rssi))
	}
	if dropAck != "" {
		f.writeLine(monitor, dropAck)
	}
}

// Commands lists the command ids received, in order (for tests).
func (f *FakeRadio) Commands() []uint16 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commands)
}
