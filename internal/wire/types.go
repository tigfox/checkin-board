// Package wire is the RC1 race-reporting message grammar: reports,
// heartbeats, gap requests and link-check probes, carried as APRS
// message text. It is pure (no I/O) and is the single source of the
// grammar; nothing else in checkin-board parses RC1 text.
//
// Ported from graywolf pkg/race (our own code). Design:
// docs/specs/2026-10-05-race-checkpoint-design.md, section 3.
package wire

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Prefix opens every RC1 message body. The trailing space is part of
// the sentinel so "RC10" or "RC1X" never match.
const Prefix = "RC1 "

// DefaultMaxTextLen is the classic APRS101 message-body limit. Operators
// on clean links may raise it (graywolf accepts up to 200 when its
// max_message_text_override is set).
const DefaultMaxTextLen = 67

// MaxBib is the largest bib number (four digits).
const MaxBib = 9999

// MaxCheckpointCodeLen bounds checkpoint codes such as "3", "AS5", "START".
const MaxCheckpointCodeLen = 6

// MaxGapSeqs caps how many sequence numbers one gap request may name,
// so a hostile "1-4000000000" can't make a node allocate gigabytes.
const MaxGapSeqs = 1000

// secondsPerDay bounds TimeOfDay.
const secondsPerDay = 24 * 60 * 60

var (
	// ErrDecode wraps every Decode failure.
	ErrDecode = errors.New("wire: decode error")
	// ErrEncode wraps every encode/pack failure.
	ErrEncode = errors.New("wire: encode error")
	// ErrInvalidBib is returned by ParseBib for operator input that is
	// not a 1-4 digit bib in 1..9999.
	ErrInvalidBib = errors.New("wire: invalid bib")
)

// Bib is a runner's bib number, 1..MaxBib.
type Bib uint16

// Valid reports whether b is within 1..MaxBib.
func (b Bib) Valid() bool { return b >= 1 && b <= MaxBib }

// ParseBib normalizes operator input (keypad, CSV) into a Bib. Leading
// zeros and surrounding whitespace are accepted ("0042" -> 42); the wire
// form is always canonical, so the decoder is stricter than this.
func ParseBib(s string) (Bib, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 4 || !allDigits(s) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidBib, s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || !Bib(n).Valid() {
		return 0, fmt.Errorf("%w: %q", ErrInvalidBib, s)
	}
	return Bib(n), nil
}

// ValidCheckpointCode reports whether s is 1..MaxCheckpointCodeLen
// characters of [A-Z0-9].
func ValidCheckpointCode(s string) bool {
	if len(s) == 0 || len(s) > MaxCheckpointCodeLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'A' || c > 'Z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// TimeOfDay is seconds since midnight UTC, 0..86399. The wire carries
// only time of day; the receiver resolves the date (see Resolve). UTC
// avoids mismatches between nodes left on UTC and browsers on local time.
type TimeOfDay int32

// Valid reports whether t is within one day.
func (t TimeOfDay) Valid() bool { return t >= 0 && t < secondsPerDay }

// TimeOfDayOf returns the UTC time of day of t, truncated to the second.
func TimeOfDayOf(t time.Time) TimeOfDay {
	u := t.UTC()
	return TimeOfDay(u.Hour()*3600 + u.Minute()*60 + u.Second())
}

// Resolve returns the absolute UTC instant with this time of day that
// is nearest to ref. Correct whenever the true instant lies within 12 h
// of ref, which covers any realistic reporting delay. t must be Valid;
// Decode only ever produces valid values.
func (t TimeOfDay) Resolve(ref time.Time) time.Time {
	r := ref.UTC()
	midnight := time.Date(r.Year(), r.Month(), r.Day(), 0, 0, 0, 0, time.UTC)
	offset := time.Duration(t) * time.Second
	best := midnight.Add(offset)
	for _, day := range []int{-1, 1} {
		cand := midnight.AddDate(0, 0, day).Add(offset)
		if absDuration(cand.Sub(r)) < absDuration(best.Sub(r)) {
			best = cand
		}
	}
	return best
}

func (t TimeOfDay) hhmm() string {
	return fmt.Sprintf("%02d%02d", t/3600, t/60%60)
}

func (t TimeOfDay) hhmmss() string {
	return fmt.Sprintf("%02d%02d%02d", t/3600, t/60%60, t%60)
}

// Entry is one bib logged at a checkpoint. Void marks a correction that
// cancels an earlier entry with the same bib and time.
type Entry struct {
	Bib  Bib
	Time TimeOfDay
	Void bool
}

// Message is one decoded RC1 message: *Report, *Heartbeat, *GapRequest,
// *Probe or *ProbeReply.
type Message interface{ rc1Kind() byte }

// Report carries a batch of entries from a checkpoint to HQ.
// Seq is the checkpoint's batch number, starting at 1.
type Report struct {
	CP      string
	Seq     uint32
	Entries []Entry
}

// Heartbeat tells HQ a checkpoint is alive, the last batch it has
// assigned (0 = none yet), and its race-clock time for skew checks.
// Closed marks a checkpoint that has closed (4.7), so HQ shows it as
// closed rather than quiet once it packs up.
type Heartbeat struct {
	CP      string
	LastSeq uint32
	Time    TimeOfDay
	Closed  bool
}

// closedFlag is the trailing heartbeat token for a closed checkpoint.
const closedFlag = "C"

// GapRequest asks a checkpoint to resend the named batches. Seqs is
// sorted and unique.
type GapRequest struct {
	CP   string
	Seqs []uint32
}

const (
	kindReport    = 'R'
	kindHeartbeat = 'H'
	kindGap       = 'G'
)

func (*Report) rc1Kind() byte     { return kindReport }
func (*Heartbeat) rc1Kind() byte  { return kindHeartbeat }
func (*GapRequest) rc1Kind() byte { return kindGap }

// IsRaceText reports whether an APRS message body is RC1 traffic. The
// inbox reader uses this as its cheap discriminator.
func IsRaceText(text string) bool { return strings.HasPrefix(text, Prefix) }

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}
