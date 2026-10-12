// Package uvk5 talks to a UV-K5 V3 / UV-K1 running the custom APRS
// firmware over its USB serial port (phase 17a): the framed command
// protocol, the monitor's text lines, and AX.25/APRS decoding of what
// the radio hears. Spec: uv-k1-k5v3-firmware-custom/claude/
// aprs-messaging-api.md, plus the additions agreed in
// aprs-firmware-request-estimate.md (requests 0-8, 10, 11).
//
// This file holds every command id and byte layout, as in the spec's
// revision 2 (protocol revision 1, 2026-10-12): sections 5.1, 5.2 and
// 5.6-5.9.
package uvk5

import (
	"fmt"
	"time"
)

// Command ids; each reply is the id + 1.
const (
	cmdHandshake    uint16 = 0x0514
	cmdSendMessage  uint16 = 0x0700
	cmdGetSettings  uint16 = 0x070C
	cmdMonitor      uint16 = 0x0710
	cmdCapabilities uint16 = 0x0712 // request 0
	cmdStatus       uint16 = 0x0714 // request 7
	cmdSetCallsign  uint16 = 0x0716 // request 8
)

// MaxText is the longest message text the firmware accepts.
const MaxText = 67

// Feature bits in the capability reply (spec 5.6).
const (
	FeatSendInfo uint32 = 1 << iota // send-fields: line number, free places, reason in the send reply
	FeatSentDrop                    // events: SENT:/DROP: lines
	FeatMultiAck                    // acks: several acks can wait
	FeatQueue                       // queue: several messages can wait; a full queue refuses
	FeatStatus                      // status: the status command
	FeatSetCall                     // set-call: the set callsign command
	FeatPath                        // path: the digipeater path setting
	FeatGap                         // gap: the transmit gap setting
	FeatRSSI                        // rssi: APRSRSSI: lines
	FeatCounters                    // counters: frame counters in the status reply
)

// Required are the features checkin-board can't work without: exact
// ack matching and a queue that never silently replaces a message.
const Required = FeatSendInfo | FeatSentDrop | FeatMultiAck | FeatQueue

// Capabilities is the 0x0713 reply (spec 5.6): Ok u8 | revision u8 |
// 2 pad | features u32 | queue depth u8 | ack depth u8 | max text u8 | pad.
type Capabilities struct {
	Revision   uint8
	Features   uint32
	QueueDepth uint8
	AckDepth   uint8
	MaxText    uint8
}

// Has reports whether every bit in f is set.
func (c Capabilities) Has(f uint32) bool { return c.Features&f == f }

func parseCapabilities(d []byte) (Capabilities, error) {
	if len(d) < 11 {
		return Capabilities{}, fmt.Errorf("uvk5: capability reply is %d bytes, want 12", len(d))
	}
	if d[0] != 1 {
		return Capabilities{}, nil // refused: revision 0
	}
	return Capabilities{Revision: d[1], Features: le32(d[4:]), QueueDepth: d[8], AckDepth: d[9], MaxText: d[10]}, nil
}

func (c Capabilities) encode() []byte {
	d := make([]byte, 12)
	d[0], d[1] = 1, c.Revision
	put32(d[4:], c.Features)
	d[8], d[9], d[10] = c.QueueDepth, c.AckDepth, c.MaxText
	return d
}

// Reason is why the radio refused or dropped something: one table for
// the send reply, DROP: lines and the status reply (spec 5.7).
type Reason uint8

const (
	ReasonNone     Reason = 0
	ReasonFull     Reason = 1  // the queue (or the ack list) is full
	ReasonSession  Reason = 2  // wrong or missing ts
	ReasonEmpty    Reason = 3  // empty destination or text
	ReasonTooLong  Reason = 4  // more than 67 text bytes
	ReasonBusy     Reason = 5  // channel or radio busy for 60 s
	ReasonNoCall   Reason = 6  // placeholder callsign
	ReasonAPRSOff  Reason = 7  // APRS is off
	ReasonBand     Reason = 8  // TX frequency outside the allowed bands
	ReasonBattery  Reason = 9  // battery too low
	ReasonMode     Reason = 10 // TX VFO not FM
	ReasonReplaced Reason = 11 // reserved: never used
	ReasonOff      Reason = 12 // APRS switched off while waiting
	ReasonInvalid  Reason = 13 // a value the radio doesn't accept (callsign, SSID)
	ReasonStore    Reason = 14 // the radio couldn't save it
	// ReasonUnknown is a DROP reason name this code doesn't know.
	ReasonUnknown Reason = 255
)

var reasonNames = map[Reason]string{
	ReasonFull: "full", ReasonSession: "session", ReasonEmpty: "empty", ReasonTooLong: "toolong",
	ReasonBusy: "busy", ReasonNoCall: "nocall", ReasonAPRSOff: "aprsoff", ReasonBand: "band",
	ReasonBattery: "battery", ReasonMode: "mode", ReasonReplaced: "replaced", ReasonOff: "off",
	ReasonInvalid: "invalid", ReasonStore: "store", ReasonUnknown: "unknown",
}

func (r Reason) String() string {
	if r == ReasonNone {
		return "ok"
	}
	if n, ok := reasonNames[r]; ok {
		return n
	}
	return fmt.Sprintf("reason-%d", uint8(r))
}

// ParseReason reads a DROP line's reason name.
func ParseReason(s string) Reason {
	for r, n := range reasonNames {
		if n == s {
			return r
		}
	}
	return ReasonUnknown
}

// SendResult is the 0x0701 reply (request 1): Ok, then the three
// formerly-padding bytes.
type SendResult struct {
	OK        bool
	Line      string // "01".."99", as on air
	FreeSlots uint8
	Reason    Reason
}

func parseSendResult(d []byte) (SendResult, error) {
	if len(d) < 4 {
		return SendResult{}, fmt.Errorf("uvk5: send reply is %d bytes, want 4", len(d))
	}
	r := SendResult{OK: d[0] == 1, FreeSlots: d[2], Reason: Reason(d[3])}
	if d[1] > 0 {
		r.Line = fmt.Sprintf("%02d", d[1])
	}
	return r, nil
}

// Settings is what checkin-board reads from the 96-byte record (5.2).
type Settings struct {
	APRSOn   bool
	Callsign string // "N0CALL-7" style, SSID 0 omitted
	Call     string // without SSID
	SSID     uint8
	// Path of messages and acks: "WIDE1-1,WIDE2-1", "WIDE1-1" or "" (direct).
	Path string
	// Gap is the shortest time between the radio's own transmissions.
	Gap time.Duration
}

// pathNames are byte 92's values.
var pathNames = []string{"WIDE1-1,WIDE2-1", "WIDE1-1", ""}

// Configured is false for the placeholder callsign.
func (s Settings) Configured() bool { return s.Call != "" && s.Call != "N0CALL" }

const settingsRecordLen = 96

func parseSettings(d []byte) (Settings, error) {
	if len(d) < 4+settingsRecordLen {
		return Settings{}, fmt.Errorf("uvk5: settings reply is %d bytes, want %d", len(d), 4+settingsRecordLen)
	}
	rec := d[4:]
	if string(rec[0:4]) != "APR1" {
		return Settings{}, fmt.Errorf("uvk5: settings record is not APR1 (not the APRS firmware)")
	}
	call := cString(rec[12:20])
	s := Settings{APRSOn: rec[5] == 1, Call: call, SSID: rec[7] & 0x0F, Path: pathNames[0], Gap: 5 * time.Second}
	s.Callsign = callsign(call, s.SSID)
	if int(rec[92]) < len(pathNames) {
		s.Path = pathNames[rec[92]]
	}
	if rec[93] != 0 {
		s.Gap = time.Duration(rec[93]) * 500 * time.Millisecond
	}
	return s, nil
}

// Status is the status reply (spec 5.8, with the counters of 11).
type Status struct {
	Monitor       bool   // monitoring is on for this port
	WouldTransmit bool   // the radio would transmit now
	Busy          bool   // a packet arriving, or squelch open
	InBand        bool   // the TX frequency is in the allowed bands
	Inhibit       Reason // why it wouldn't transmit now (ReasonNone: it would)
	BatteryMV     uint16
	Decoded       uint16 // frames decoded since APRS was switched on (wraps)
	FreqHz        uint32 // the main VFO's receive frequency
	Damaged       uint16 // packet-like bursts that failed the checksum (approximate; wraps)
	Sent          uint16 // packets transmitted since power-up (wraps)
	FreeSlots     uint8
	AcksWaiting   uint8
}

const (
	statusMonitor = 1 << iota
	statusWouldTX
	statusBusy
	statusInBand
)

func parseStatus(d []byte) (Status, error) {
	if len(d) < 18 || d[0] != 1 {
		return Status{}, fmt.Errorf("uvk5: status reply is %d bytes or refused", len(d))
	}
	f := d[1]
	return Status{Monitor: f&statusMonitor != 0, WouldTransmit: f&statusWouldTX != 0, Busy: f&statusBusy != 0,
		InBand: f&statusInBand != 0, Inhibit: Reason(d[2]), BatteryMV: le16(d[4:]), Decoded: le16(d[6:]),
		FreqHz: le32(d[8:]) * 10, Damaged: le16(d[12:]), Sent: le16(d[14:]), FreeSlots: d[16], AcksWaiting: d[17]}, nil
}

func (s Status) encode() []byte {
	d := make([]byte, 20)
	d[0] = 1
	var f uint8
	for bit, on := range map[uint8]bool{statusMonitor: s.Monitor, statusWouldTX: s.WouldTransmit, statusBusy: s.Busy, statusInBand: s.InBand} {
		if on {
			f |= bit
		}
	}
	d[1], d[2] = f, uint8(s.Inhibit)
	put16(d[4:], s.BatteryMV)
	put16(d[6:], s.Decoded)
	put32(d[8:], s.FreqHz/10)
	put16(d[12:], s.Damaged)
	put16(d[14:], s.Sent)
	d[16], d[17] = s.FreeSlots, s.AcksWaiting
	return d
}

// setCallsignData is the set-callsign command's data (spec 5.9): ts u32 |
// callsign 8 bytes (NUL-padded, 1-6 characters) | SSID u8.
func setCallsignData(ts uint32, call string, ssid uint8) []byte {
	d := make([]byte, 13)
	put32(d, ts)
	copy(d[4:10], call)
	d[12] = ssid
	return d
}

func le16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func put16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }
func put32(b []byte, v uint32) {
	b[0], b[1], b[2], b[3] = byte(v), byte(v>>8), byte(v>>16), byte(v>>24)
}
func b2u(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

func callsign(call string, ssid uint8) string {
	if ssid == 0 {
		return call
	}
	return fmt.Sprintf("%s-%d", call, ssid)
}
