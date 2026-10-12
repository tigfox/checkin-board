// Package uvk5 talks to a UV-K5 V3 / UV-K1 running the custom APRS
// firmware over its USB serial port (phase 17a): the framed command
// protocol, the monitor's text lines, and AX.25/APRS decoding of what
// the radio hears. Spec: uv-k1-k5v3-firmware-custom/claude/
// aprs-messaging-api.md, plus the additions agreed in
// aprs-firmware-request-estimate.md (requests 0-8, 10, 11).
//
// This file holds every command id and byte layout. Layouts marked
// ASSUMED were not fixed by the firmware's documents when this was
// written (2026-10-12): they follow checkin-board's reply, and only this
// file changes when the firmware's spec revision lands.
package uvk5

import "fmt"

// Command ids; each reply is the id + 1.
const (
	cmdHandshake    uint16 = 0x0514
	cmdSendMessage  uint16 = 0x0700
	cmdGetSettings  uint16 = 0x070C
	cmdMonitor      uint16 = 0x0710
	cmdCapabilities uint16 = 0x0712 // agreed (request 0)
	cmdStatus       uint16 = 0x0714 // ASSUMED id (request 7)
	cmdSetCallsign  uint16 = 0x0716 // ASSUMED id (request 8)
)

// MaxText is the longest message text the firmware accepts.
const MaxText = 67

// Feature bits in the capability reply. ASSUMED bit positions; the
// meaning of each is agreed.
const (
	FeatSendInfo uint32 = 1 << iota // request 1: line number, free slots, reason in the send reply
	FeatSentDrop                    // request 2: SENT:/DROP: lines
	FeatMultiAck                    // request 3: several pending acks
	FeatQueue                       // request 4: outgoing queue, refuses when full
	FeatStatus                      // request 7: status command
	FeatCounters                    // request 11: frame counters in the status reply
	FeatSetCall                     // request 8: set callsign/SSID
	FeatPath                        // request 5: digipeater path setting
	FeatRSSI                        // request 10: APRSRSSI: lines
	FeatGap                         // request 6: transmit gap setting
)

// Required are the features checkin-board can't work without: exact
// ack matching and a queue that never silently replaces a message.
const Required = FeatSendInfo | FeatSentDrop | FeatMultiAck | FeatQueue

// Capabilities is the 0x0713 reply. ASSUMED layout:
//
//	rev u8 | pad u8 | features u32 | queue depth u8 | ack depth u8 | max text u8
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
	if len(d) < 9 {
		return Capabilities{}, fmt.Errorf("uvk5: capability reply is %d bytes, want 9", len(d))
	}
	return Capabilities{Revision: d[0], Features: le32(d[2:]), QueueDepth: d[6], AckDepth: d[7], MaxText: d[8]}, nil
}

func (c Capabilities) encode() []byte {
	d := make([]byte, 9)
	d[0] = c.Revision
	put32(d[2:], c.Features)
	d[6], d[7], d[8] = c.QueueDepth, c.AckDepth, c.MaxText
	return d
}

// Reason is why the radio refused or dropped something (request 2's
// shared reason table). ASSUMED codes; the names are the DROP lines'.
type Reason uint8

const (
	ReasonNone     Reason = iota
	ReasonFull            // the queue is full
	ReasonBusy            // no clear channel for 60 s
	ReasonNoCall          // placeholder callsign
	ReasonAPRSOff         // APRS is off
	ReasonBand            // the VFO is outside the allowed bands
	ReasonBattery         // battery low
	ReasonReplaced        // replaced (not expected with a queue)
	ReasonOff             // APRS switched off with it pending
	ReasonRefused         // refused for another reason (bad ts, empty text)
)

var reasonNames = map[Reason]string{
	ReasonNone: "", ReasonFull: "full", ReasonBusy: "busy", ReasonNoCall: "nocall", ReasonAPRSOff: "aprsoff",
	ReasonBand: "band", ReasonBattery: "battery", ReasonReplaced: "replaced", ReasonOff: "off", ReasonRefused: "refused",
}

func (r Reason) String() string {
	if n, ok := reasonNames[r]; ok {
		return n
	}
	return fmt.Sprintf("reason-%d", uint8(r))
}

// ParseReason reads a DROP line's reason name.
func ParseReason(s string) Reason {
	for r, n := range reasonNames {
		if n == s && n != "" {
			return r
		}
	}
	return ReasonRefused
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

// Settings is what checkin-board reads from the 96-byte record.
type Settings struct {
	APRSOn   bool
	Callsign string // "N0CALL-7" style, SSID 0 omitted
	Call     string // without SSID
	SSID     uint8
}

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
	s := Settings{APRSOn: rec[5] == 1, Call: call, SSID: rec[7] & 0x0F}
	s.Callsign = callsign(call, s.SSID)
	return s, nil
}

// Status is the status reply (requests 7 and 11). ASSUMED layout:
//
//	ok u8 | low battery u8 | in band u8 | inhibit reason u8 | battery mV u16 | busy u8 | monitor u8 |
//	frequency Hz u32 | decoded u32 | bad FCS u32 | sent u32
type Status struct {
	LowBattery bool
	InBand     bool
	Inhibit    Reason // why it would refuse to transmit now (ReasonNone: it wouldn't)
	BatteryMV  uint16
	Busy       bool
	Monitor    bool
	FreqHz     uint32
	Decoded    uint32
	BadFCS     uint32
	Sent       uint32
}

func parseStatus(d []byte) (Status, error) {
	if len(d) < 24 || d[0] != 1 {
		return Status{}, fmt.Errorf("uvk5: status reply is %d bytes or refused", len(d))
	}
	return Status{LowBattery: d[1] == 1, InBand: d[2] == 1, Inhibit: Reason(d[3]), BatteryMV: le16(d[4:]),
		Busy: d[6] == 1, Monitor: d[7] == 1, FreqHz: le32(d[8:]), Decoded: le32(d[12:]), BadFCS: le32(d[16:]),
		Sent: le32(d[20:])}, nil
}

func (s Status) encode() []byte {
	d := make([]byte, 24)
	d[0] = 1
	d[1], d[2], d[3] = b2u(s.LowBattery), b2u(s.InBand), uint8(s.Inhibit)
	put16(d[4:], s.BatteryMV)
	d[6], d[7] = b2u(s.Busy), b2u(s.Monitor)
	put32(d[8:], s.FreqHz)
	put32(d[12:], s.Decoded)
	put32(d[16:], s.BadFCS)
	put32(d[20:], s.Sent)
	return d
}

// setCallsignData is the set-callsign command's data. ASSUMED layout:
// ts u32 | callsign 7 bytes (NUL-padded, 1-6 chars) | ssid u8.
func setCallsignData(ts uint32, call string, ssid uint8) []byte {
	d := make([]byte, 12)
	put32(d, ts)
	copy(d[4:11], call)
	d[11] = ssid
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
