package uvk5

import (
	"encoding/hex"
	"strconv"
	"strings"
)

// Monitor line events (spec section 6 and requests 2, 10).

// Heard is a frame the radio decoded (APRSRAW:).
type Heard struct {
	Packet Packet
}

// HeardRSSI is the signal strength of the frame just heard
// (APRSRSSI:, the line straight after its APRSRAW:).
type HeardRSSI struct {
	DBm int
}

// Sent says a queued message went on air (SENT:<to>{nn}).
type Sent struct {
	To, Line string
}

// Dropped says a queued message, or a pending ack, was given up
// (DROP:<to>{nn} <reason>, or DROP:ack <to>{id} <reason>).
type Dropped struct {
	To, Line string
	Ack      bool
	Reason   Reason
}

// OtherLine is any other line (APRS:, DIGI:, unknown).
type OtherLine struct {
	Text string
}

// ParseLine reads one monitor line into one of the types above.
func ParseLine(s string) any {
	switch {
	case strings.HasPrefix(s, "APRSRAW:"):
		raw, err := hex.DecodeString(strings.TrimSpace(s[len("APRSRAW:"):]))
		if err != nil {
			return OtherLine{s}
		}
		p, err := DecodeAX25(raw)
		if err != nil {
			return OtherLine{s}
		}
		return Heard{Packet: p}
	case strings.HasPrefix(s, "APRSRSSI:"):
		n, err := strconv.Atoi(strings.TrimSpace(s[len("APRSRSSI:"):]))
		if err != nil {
			return OtherLine{s}
		}
		return HeardRSSI{DBm: n}
	case strings.HasPrefix(s, "SENT:"):
		if to, line, ok := toAndLine(s[len("SENT:"):]); ok {
			return Sent{To: to, Line: line}
		}
	case strings.HasPrefix(s, "DROP:"):
		rest := s[len("DROP:"):]
		ack := false
		if r, ok := strings.CutPrefix(rest, "ack "); ok {
			rest, ack = r, true
		}
		target, reason, _ := strings.Cut(rest, " ")
		if to, line, ok := toAndLine(target); ok {
			return Dropped{To: to, Line: line, Ack: ack, Reason: ParseReason(strings.TrimSpace(reason))}
		}
	}
	return OtherLine{s}
}

// toAndLine splits "KD2DCM-1{07}" (closing brace optional).
func toAndLine(s string) (string, string, bool) {
	to, line, ok := strings.Cut(strings.TrimSpace(s), "{")
	line = strings.TrimSuffix(line, "}")
	if !ok || to == "" || line == "" {
		return "", "", false
	}
	return strings.ToUpper(to), line, true
}

// FormatHeard renders an APRSRAW: line (for the fake radio).
func FormatHeard(raw []byte) string { return "APRSRAW:" + strings.ToUpper(hex.EncodeToString(raw)) }
