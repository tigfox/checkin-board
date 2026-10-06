package wire

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PackReport encodes as many leading entries as fit in maxLen characters
// and returns the text plus how many entries it consumed. Entries keep
// their input order; a new "@HHMM" group starts whenever the minute
// changes. The caller re-batches entries[n:] under the next seq.
//
// It fails if the first entry can't fit or if any entry it reaches is
// invalid, so a bad row is never silently skipped.
func PackReport(cp string, seq uint32, entries []Entry, maxLen int) (string, int, error) {
	if !ValidCheckpointCode(cp) {
		return "", 0, fmt.Errorf("%w: invalid checkpoint code %q", ErrEncode, cp)
	}
	if seq == 0 {
		return "", 0, fmt.Errorf("%w: seq must be >= 1", ErrEncode)
	}
	if len(entries) == 0 {
		return "", 0, fmt.Errorf("%w: no entries", ErrEncode)
	}

	var b strings.Builder
	b.WriteString(Prefix)
	b.WriteByte(kindReport)
	b.WriteString(" " + cp + " " + strconv.FormatUint(uint64(seq), 10))

	n := 0
	curMinute := TimeOfDay(-1)
	for _, e := range entries {
		if !e.Bib.Valid() || !e.Time.Valid() {
			return "", 0, fmt.Errorf("%w: invalid entry %+v", ErrEncode, e)
		}
		chunk := entryToken(e)
		minute := e.Time / 60
		if minute != curMinute {
			chunk = " @" + e.Time.hhmm() + chunk
		}
		if b.Len()+len(chunk) > maxLen {
			break
		}
		b.WriteString(chunk)
		curMinute = minute
		n++
	}
	if n == 0 {
		return "", 0, fmt.Errorf("%w: first entry does not fit in %d chars", ErrEncode, maxLen)
	}
	return b.String(), n, nil
}

// entryToken renders " [-]bib/ss".
func entryToken(e Entry) string {
	void := ""
	if e.Void {
		void = "-"
	}
	return fmt.Sprintf(" %s%d/%02d", void, e.Bib, e.Time%60)
}

// EncodeHeartbeat renders "RC1 H <cp> <lastseq> <HHMMSS>".
func EncodeHeartbeat(h Heartbeat) (string, error) {
	if !ValidCheckpointCode(h.CP) {
		return "", fmt.Errorf("%w: invalid checkpoint code %q", ErrEncode, h.CP)
	}
	if !h.Time.Valid() {
		return "", fmt.Errorf("%w: invalid time %d", ErrEncode, h.Time)
	}
	return fmt.Sprintf("%s%c %s %d %s", Prefix, kindHeartbeat, h.CP, h.LastSeq, h.Time.hhmmss()), nil
}

// PackGap encodes a gap request naming as many of seqs as fit in maxLen
// characters (and at most MaxGapSeqs), compressing runs into "a-b"
// ranges. Input order and duplicates don't matter; the input slice is
// not modified. rest holds the sorted seqs that didn't fit, to be sent
// in a later request. Ranges are never split to fit maxLen; at the
// 67-char default even the longest range token fits.
func PackGap(cp string, seqs []uint32, maxLen int) (text string, rest []uint32, err error) {
	if !ValidCheckpointCode(cp) {
		return "", nil, fmt.Errorf("%w: invalid checkpoint code %q", ErrEncode, cp)
	}
	sorted := slices.Compact(slices.Sorted(slices.Values(seqs)))
	if len(sorted) == 0 {
		return "", nil, fmt.Errorf("%w: no seqs", ErrEncode)
	}
	if sorted[0] == 0 {
		return "", nil, fmt.Errorf("%w: seq 0 is not a valid batch", ErrEncode)
	}

	var b strings.Builder
	b.WriteString(Prefix)
	b.WriteByte(kindGap)
	b.WriteString(" " + cp + " ")
	headerLen := b.Len()

	count, i := 0, 0
	for i < len(sorted) && count < MaxGapSeqs {
		j := runEnd(sorted, i)
		// Never let one request expand past MaxGapSeqs; the tail of an
		// oversized run waits for the next request.
		if limit := i + (MaxGapSeqs - count) - 1; j > limit {
			j = limit
		}
		chunk := rangeToken(sorted[i], sorted[j])
		if b.Len() > headerLen {
			chunk = "," + chunk
		}
		if b.Len()+len(chunk) > maxLen {
			break
		}
		b.WriteString(chunk)
		count += j - i + 1
		i = j + 1
	}
	if count == 0 {
		return "", nil, fmt.Errorf("%w: first seq does not fit in %d chars", ErrEncode, maxLen)
	}
	if i < len(sorted) {
		rest = slices.Clone(sorted[i:])
	}
	return b.String(), rest, nil
}

// runEnd returns the index of the last element of the consecutive run
// starting at sorted[i].
func runEnd(sorted []uint32, i int) int {
	j := i
	for j+1 < len(sorted) && sorted[j+1] == sorted[j]+1 {
		j++
	}
	return j
}

func rangeToken(lo, hi uint32) string {
	if lo == hi {
		return strconv.FormatUint(uint64(lo), 10)
	}
	return strconv.FormatUint(uint64(lo), 10) + "-" + strconv.FormatUint(uint64(hi), 10)
}
