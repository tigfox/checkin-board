package wire

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Decode parses an RC1 message body. It returns *Report, *Heartbeat,
// *GapRequest, *Probe or *ProbeReply, or a nil Message and an error
// wrapping ErrDecode.
//
// Decode is text-canonical: it accepts only the exact spelling the
// encoders produce (single spaces, no leading zeros, sorted gap lists,
// no redundant groups). Every accepted body therefore re-encodes to the
// identical string, so dedup keyed on message text can't be fooled by
// an alternate spelling of the same batch. FuzzDecode enforces this.
func Decode(text string) (Message, error) {
	if !IsRaceText(text) {
		return nil, decodeErr("missing %q prefix", Prefix)
	}
	fields := strings.Split(text[len(Prefix):], " ")
	if slices.Contains(fields, "") {
		return nil, decodeErr("fields must be separated by single spaces")
	}
	if len(fields) < 2 {
		return nil, decodeErr("too few fields")
	}
	kind, cp, args := fields[0], fields[1], fields[2:]
	if !ValidCheckpointCode(cp) {
		return nil, decodeErr("invalid checkpoint code %q", cp)
	}
	switch kind {
	case string(rune(kindReport)):
		return decodeReport(cp, args)
	case string(rune(kindHeartbeat)):
		return decodeHeartbeat(cp, args)
	case string(rune(kindGap)):
		return decodeGap(cp, args)
	case string(rune(kindProbe)):
		return decodeProbe(cp, args)
	case string(rune(kindProbeReply)):
		return decodeProbeReply(cp, args)
	default:
		return nil, decodeErr("unknown message type %q", kind)
	}
}

func decodeReport(cp string, args []string) (Message, error) {
	if len(args) < 3 {
		return nil, decodeErr("report needs seq, a group, and an entry")
	}
	seq, err := parseSeq(args[0])
	if err != nil {
		return nil, err
	}
	if seq == 0 {
		return nil, decodeErr("report seq must be >= 1")
	}

	var entries []Entry
	groupStart := TimeOfDay(-1)
	groupHasEntry := true
	for _, tok := range args[1:] {
		if strings.HasPrefix(tok, "@") {
			if !groupHasEntry {
				return nil, decodeErr("empty group before %q", tok)
			}
			m, err := parseHHMM(tok[1:])
			if err != nil {
				return nil, err
			}
			if m == groupStart {
				return nil, decodeErr("redundant group %q", tok)
			}
			groupStart, groupHasEntry = m, false
			continue
		}
		if groupStart < 0 {
			return nil, decodeErr("entry %q before any @HHMM group", tok)
		}
		e, err := parseEntry(tok, groupStart)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
		groupHasEntry = true
	}
	if !groupHasEntry {
		return nil, decodeErr("trailing empty group")
	}
	return &Report{CP: cp, Seq: seq, Entries: entries}, nil
}

// parseEntry parses "[-]bib/ss" within the minute starting at groupStart.
func parseEntry(tok string, groupStart TimeOfDay) (Entry, error) {
	void := strings.HasPrefix(tok, "-")
	body := strings.TrimPrefix(tok, "-")
	bibStr, ss, ok := strings.Cut(body, "/")
	if !ok {
		return Entry{}, decodeErr("entry %q missing /ss", tok)
	}
	bib, err := parseCanonicalBib(bibStr)
	if err != nil {
		return Entry{}, err
	}
	if len(ss) != 2 || !allDigits(ss) {
		return Entry{}, decodeErr("entry %q seconds must be 2 digits", tok)
	}
	sec, _ := strconv.Atoi(ss)
	if sec > 59 {
		return Entry{}, decodeErr("entry %q seconds out of range", tok)
	}
	return Entry{Bib: bib, Time: groupStart + TimeOfDay(sec), Void: void}, nil
}

// parseCanonicalBib accepts only the wire form: 1-4 digits, no leading
// zero. One spelling per bib keeps HQ-side matching exact.
func parseCanonicalBib(s string) (Bib, error) {
	if s == "" || len(s) > 4 || !allDigits(s) || s[0] == '0' {
		return 0, decodeErr("invalid bib %q", s)
	}
	n, _ := strconv.Atoi(s)
	return Bib(n), nil
}

// parseHHMM returns the start of the minute as a TimeOfDay.
func parseHHMM(s string) (TimeOfDay, error) {
	if len(s) != 4 || !allDigits(s) {
		return 0, decodeErr("group %q must be HHMM", s)
	}
	h, _ := strconv.Atoi(s[:2])
	m, _ := strconv.Atoi(s[2:])
	if h > 23 || m > 59 {
		return 0, decodeErr("group %q out of range", s)
	}
	return TimeOfDay(h*3600 + m*60), nil
}

func decodeHeartbeat(cp string, args []string) (Message, error) {
	closed := false
	if len(args) == 3 && args[2] == closedFlag {
		closed, args = true, args[:2]
	}
	if len(args) != 2 {
		return nil, decodeErr("heartbeat needs lastseq and HHMMSS, then optionally C")
	}
	last, err := parseSeq(args[0])
	if err != nil {
		return nil, err
	}
	s := args[1]
	if len(s) != 6 || !allDigits(s) {
		return nil, decodeErr("heartbeat time %q must be HHMMSS", s)
	}
	minute, err := parseHHMM(s[:4])
	if err != nil {
		return nil, err
	}
	sec, _ := strconv.Atoi(s[4:])
	if sec > 59 {
		return nil, decodeErr("heartbeat time %q out of range", s)
	}
	return &Heartbeat{CP: cp, LastSeq: last, Time: minute + TimeOfDay(sec), Closed: closed}, nil
}

func decodeGap(cp string, args []string) (Message, error) {
	if len(args) != 1 {
		return nil, decodeErr("gap request needs exactly one seq list")
	}
	seqs, err := parseSeqList(args[0], math.MaxUint32, MaxGapSeqs)
	if err != nil {
		return nil, err
	}
	return &GapRequest{CP: cp, Seqs: seqs}, nil
}

// parseSeqList parses a canonical "a,b-c" list: ranges only for runs
// (lo < hi), elements strictly ascending with a gap between them
// (adjacent runs would have been merged by the encoder), every value in
// 1..maxValue, and at most maxCount values in total.
func parseSeqList(list string, maxValue uint64, maxCount int) ([]uint32, error) {
	var seqs []uint32
	var prevHi uint64 // 0 = none yet; values start at 1
	for _, part := range strings.Split(list, ",") {
		loStr, hiStr, isRange := strings.Cut(part, "-")
		lo, err := parseSeq(loStr)
		if err != nil {
			return nil, err
		}
		hi := lo
		if isRange {
			if hi, err = parseSeq(hiStr); err != nil {
				return nil, err
			}
		}
		if lo == 0 || (isRange && hi <= lo) || uint64(hi) > maxValue {
			return nil, decodeErr("invalid list element %q", part)
		}
		if prevHi != 0 && uint64(lo) <= prevHi+1 {
			return nil, decodeErr("list element %q not ascending and disjoint", part)
		}
		prevHi = uint64(hi)
		if uint64(len(seqs))+uint64(hi-lo)+1 > uint64(maxCount) {
			return nil, decodeErr("list names more than %d values", maxCount)
		}
		for s := uint64(lo); s <= uint64(hi); s++ {
			seqs = append(seqs, uint32(s))
		}
	}
	return seqs, nil
}

func parseSeq(s string) (uint32, error) {
	if s == "" || !allDigits(s) || (len(s) > 1 && s[0] == '0') {
		return 0, decodeErr("invalid seq %q", s)
	}
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, decodeErr("seq %q out of range", s)
	}
	return uint32(n), nil
}

func decodeErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrDecode}, args...)...)
}
