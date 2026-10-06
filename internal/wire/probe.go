package wire

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Link-check limits (spec section 4.8).
const (
	// MaxProbeRun bounds the prober-chosen run number.
	MaxProbeRun = 9999
	// MaxProbes bounds how many probes one run may send.
	MaxProbes = 20
	// MinLevel is the lowest receive level a reply can carry, in dBFS.
	MinLevel Level = -99
	// LevelUnknown marks a reply whose responder had no level reading
	// (e.g. the frame came from a hardware TNC). Encoded as "X".
	LevelUnknown Level = 1
)

const (
	kindProbe      = 'P'
	kindProbeReply = 'Q'

	viaDirect    = "-"
	levelUnknown = "X"
)

// viaRe is a digipeater callsign or alias with an optional SSID.
var viaRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-[A-Z0-9]{1,2})?$`)

// Level is a receive audio level in whole dBFS, MinLevel..0, or
// LevelUnknown. The zero value means 0 dBFS (full scale), not unknown:
// set LevelUnknown explicitly when there is no reading.
type Level int8

// Valid reports whether l is in range or LevelUnknown.
func (l Level) Valid() bool { return l == LevelUnknown || (l >= MinLevel && l <= 0) }

// Probe is one link-check probe. CP is the prober's station code.
type Probe struct {
	CP    string
	Run   uint16
	Index uint8
	Total uint8
}

// ProbeReply summarizes what the responder heard of one run. Heard is
// sorted and unique; Via is "" for a direct path.
type ProbeReply struct {
	CP    string
	Run   uint16
	Heard []uint8
	Level Level
	Via   string
}

func (*Probe) rc1Kind() byte      { return kindProbe }
func (*ProbeReply) rc1Kind() byte { return kindProbeReply }

// EncodeProbe renders "RC1 P <cp> <run> <i>/<n>".
func EncodeProbe(p Probe) (string, error) {
	if !ValidCheckpointCode(p.CP) {
		return "", fmt.Errorf("%w: invalid checkpoint code %q", ErrEncode, p.CP)
	}
	if p.Run == 0 || p.Run > MaxProbeRun {
		return "", fmt.Errorf("%w: probe run %d outside 1..%d", ErrEncode, p.Run, MaxProbeRun)
	}
	if p.Total == 0 || p.Total > MaxProbes || p.Index == 0 || p.Index > p.Total {
		return "", fmt.Errorf("%w: probe %d/%d invalid (max %d)", ErrEncode, p.Index, p.Total, MaxProbes)
	}
	return fmt.Sprintf("%s%c %s %d %d/%d", Prefix, kindProbe, p.CP, p.Run, p.Index, p.Total), nil
}

// EncodeProbeReply renders "RC1 Q <cp> <run> <heard> <lvl> <via>". The
// heard list may be in any order with duplicates; the input slice is
// not modified.
func EncodeProbeReply(r ProbeReply) (string, error) {
	if !ValidCheckpointCode(r.CP) {
		return "", fmt.Errorf("%w: invalid checkpoint code %q", ErrEncode, r.CP)
	}
	if r.Run == 0 || r.Run > MaxProbeRun {
		return "", fmt.Errorf("%w: probe run %d outside 1..%d", ErrEncode, r.Run, MaxProbeRun)
	}
	heard := make([]uint32, 0, len(r.Heard))
	for _, i := range r.Heard {
		if i == 0 || i > MaxProbes {
			return "", fmt.Errorf("%w: heard index %d outside 1..%d", ErrEncode, i, MaxProbes)
		}
		heard = append(heard, uint32(i))
	}
	heard = slices.Compact(slices.Sorted(slices.Values(heard)))
	if len(heard) == 0 {
		return "", fmt.Errorf("%w: reply must name at least one heard probe", ErrEncode)
	}
	if !r.Level.Valid() {
		return "", fmt.Errorf("%w: level %d outside %d..0", ErrEncode, r.Level, MinLevel)
	}
	via := viaDirect
	if r.Via != "" {
		if !viaRe.MatchString(r.Via) {
			return "", fmt.Errorf("%w: invalid via %q", ErrEncode, r.Via)
		}
		via = r.Via
	}
	return fmt.Sprintf("%s%c %s %d %s %s %s", Prefix, kindProbeReply, r.CP, r.Run, seqListToken(heard), r.Level.token(), via), nil
}

func (l Level) token() string {
	if l == LevelUnknown {
		return levelUnknown
	}
	return strconv.Itoa(int(l))
}

// seqListToken renders sorted unique values as "a-b,c" with runs
// compressed, the same canonical form gap requests use.
func seqListToken(sorted []uint32) string {
	parts := make([]string, 0, len(sorted))
	for i := 0; i < len(sorted); {
		j := runEnd(sorted, i)
		parts = append(parts, rangeToken(sorted[i], sorted[j]))
		i = j + 1
	}
	return strings.Join(parts, ",")
}

func decodeProbe(cp string, args []string) (Message, error) {
	if len(args) != 2 {
		return nil, decodeErr("probe needs exactly run and i/n")
	}
	run, err := parseRun(args[0])
	if err != nil {
		return nil, err
	}
	iStr, nStr, ok := strings.Cut(args[1], "/")
	if !ok {
		return nil, decodeErr("probe %q must be i/n", args[1])
	}
	idx, err := parseSmall(iStr, MaxProbes)
	if err != nil {
		return nil, err
	}
	total, err := parseSmall(nStr, MaxProbes)
	if err != nil {
		return nil, err
	}
	if idx > total {
		return nil, decodeErr("probe index %d > total %d", idx, total)
	}
	return &Probe{CP: cp, Run: run, Index: uint8(idx), Total: uint8(total)}, nil
}

func decodeProbeReply(cp string, args []string) (Message, error) {
	if len(args) != 4 {
		return nil, decodeErr("probe reply needs exactly run, heard, level and via")
	}
	run, err := parseRun(args[0])
	if err != nil {
		return nil, err
	}
	seqs, err := parseSeqList(args[1], MaxProbes, MaxProbes)
	if err != nil {
		return nil, err
	}
	heard := make([]uint8, len(seqs))
	for i, s := range seqs {
		heard[i] = uint8(s)
	}
	level, err := parseLevel(args[2])
	if err != nil {
		return nil, err
	}
	via := ""
	if args[3] != viaDirect {
		if !viaRe.MatchString(args[3]) {
			return nil, decodeErr("invalid via %q", args[3])
		}
		via = args[3]
	}
	return &ProbeReply{CP: cp, Run: run, Heard: heard, Level: level, Via: via}, nil
}

func parseRun(s string) (uint16, error) {
	n, err := parseSmall(s, MaxProbeRun)
	return uint16(n), err
}

// parseSmall parses a canonical integer in 1..limit.
func parseSmall(s string, limit int) (int, error) {
	if s == "" || len(s) > 4 || !allDigits(s) || s[0] == '0' {
		return 0, decodeErr("invalid number %q", s)
	}
	n, _ := strconv.Atoi(s)
	if n > limit {
		return 0, decodeErr("number %q above %d", s, limit)
	}
	return n, nil
}

// parseLevel accepts "X", "0", or "-N" with N in 1..99, no leading zero.
func parseLevel(s string) (Level, error) {
	switch {
	case s == levelUnknown:
		return LevelUnknown, nil
	case s == "0":
		return 0, nil
	case len(s) >= 2 && len(s) <= 3 && s[0] == '-' && allDigits(s[1:]) && s[1] != '0':
		n, _ := strconv.Atoi(s[1:])
		return Level(-n), nil
	default:
		return 0, decodeErr("invalid level %q", s)
	}
}
