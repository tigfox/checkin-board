package uvk5

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Packet is a decoded AX.25 UI frame (no FCS), as on an APRSRAW: line.
type Packet struct {
	Dest, Src string   // "CALL" or "CALL-SSID"
	Path      []string // digipeaters, "*" marking one that has repeated it
	Info      []byte
}

const (
	addrLen     = 7
	maxDigis    = 8
	ctrlUI      = 0x03
	pidNoLayer3 = 0xF0
)

// DecodeAX25 reads a UI frame: destination, source, 0-8 digipeaters
// (the last address has bit 0 of its SSID byte set), control 03, PID F0,
// then the information field.
func DecodeAX25(b []byte) (Packet, error) {
	var addrs []string
	i := 0
	for {
		if i+addrLen > len(b) {
			return Packet{}, errors.New("uvk5: AX.25 frame ends inside its addresses")
		}
		a, last, repeated := decodeAddress(b[i : i+addrLen])
		if repeated && len(addrs) >= 2 {
			a += "*"
		}
		addrs = append(addrs, a)
		i += addrLen
		if last {
			break
		}
		if len(addrs) == 2+maxDigis {
			return Packet{}, errors.New("uvk5: AX.25 frame has more than 8 digipeaters")
		}
	}
	if len(addrs) < 2 {
		return Packet{}, errors.New("uvk5: AX.25 frame has no source address")
	}
	if i+2 > len(b) || b[i] != ctrlUI || b[i+1] != pidNoLayer3 {
		return Packet{}, errors.New("uvk5: not an AX.25 UI frame")
	}
	return Packet{Dest: addrs[0], Src: addrs[1], Path: addrs[2:], Info: append([]byte(nil), b[i+2:]...)}, nil
}

func decodeAddress(a []byte) (call string, last, repeated bool) {
	var sb strings.Builder
	for _, c := range a[:6] {
		if ch := c >> 1; ch != ' ' {
			sb.WriteByte(ch)
		}
	}
	ssid := (a[6] >> 1) & 0x0F
	return callsign(sb.String(), ssid), a[6]&0x01 != 0, a[6]&0x80 != 0
}

// EncodeAX25 builds a UI frame (for the fake radio and tests).
func EncodeAX25(p Packet) ([]byte, error) {
	addrs := append([]string{p.Dest, p.Src}, p.Path...)
	out := make([]byte, 0, len(addrs)*addrLen+2+len(p.Info))
	for i, a := range addrs {
		enc, err := encodeAddress(a, i == len(addrs)-1)
		if err != nil {
			return nil, err
		}
		out = append(out, enc...)
	}
	out = append(out, ctrlUI, pidNoLayer3)
	return append(out, p.Info...), nil
}

func encodeAddress(a string, last bool) ([]byte, error) {
	repeated := strings.HasSuffix(a, "*")
	a = strings.TrimSuffix(a, "*")
	call, ssid, err := SplitCallsign(a)
	if err != nil {
		return nil, err
	}
	out := make([]byte, addrLen)
	for i := range 6 {
		c := byte(' ')
		if i < len(call) {
			c = call[i]
		}
		out[i] = c << 1
	}
	out[6] = 0x60 | ssid<<1
	if last {
		out[6] |= 0x01
	}
	if repeated {
		out[6] |= 0x80
	}
	return out, nil
}

// SplitCallsign splits "CALL-SSID" (SSID 0-15; none means 0).
func SplitCallsign(s string) (string, uint8, error) {
	call, ssidText, hasSSID := strings.Cut(strings.ToUpper(strings.TrimSpace(s)), "-")
	if call == "" || len(call) > 6 {
		return "", 0, fmt.Errorf("uvk5: callsign %q must be 1-6 characters", s)
	}
	for _, c := range call {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", 0, fmt.Errorf("uvk5: callsign %q has a character other than A-Z 0-9", s)
		}
	}
	if !hasSSID {
		return call, 0, nil
	}
	n, err := strconv.Atoi(ssidText)
	if err != nil || n < 0 || n > 15 {
		return "", 0, fmt.Errorf("uvk5: SSID in %q must be 0-15", s)
	}
	return call, uint8(n), nil
}

// Message is an APRS message, ack or rej (APRS101 chapter 14).
type Message struct {
	Addressee string // trimmed
	Text      string // empty for an ack or rej
	ID        string // message id ({id} on a message; the acked id on an ack/rej)
	Ack, Rej  bool
}

// ParseMessage reads an information field ":ADDRESSEE:text{id".
func ParseMessage(info []byte) (Message, bool) {
	if len(info) < 11 || info[0] != ':' || info[10] != ':' {
		return Message{}, false
	}
	m := Message{Addressee: strings.TrimSpace(string(info[1:10]))}
	body := string(info[11:])
	for _, kind := range []string{"ack", "rej"} {
		if id, ok := strings.CutPrefix(body, kind); ok && id != "" && len(id) <= 5 && !strings.ContainsAny(id, " {") {
			m.ID = strings.TrimRight(id, "\r\n}")
			m.Ack, m.Rej = kind == "ack", kind == "rej"
			return m, true
		}
	}
	if i := strings.LastIndexByte(body, '{'); i >= 0 {
		id := body[i+1:]
		if j := strings.IndexByte(id, '}'); j >= 0 { // APRS 1.1 reply-ack "{mm}aa"
			id = id[:j]
		}
		if id != "" && len(id) <= 5 {
			m.Text, m.ID = body[:i], id
			return m, true
		}
	}
	m.Text = body
	return m, true
}

// FormatMessage renders a message, ack or rej information field.
func FormatMessage(m Message) []byte {
	to := fmt.Sprintf("%-9s", m.Addressee)
	switch {
	case m.Ack:
		return []byte(":" + to + ":ack" + m.ID)
	case m.Rej:
		return []byte(":" + to + ":rej" + m.ID)
	case m.ID != "":
		return []byte(":" + to + ":" + m.Text + "{" + m.ID)
	default:
		return []byte(":" + to + ":" + m.Text)
	}
}
