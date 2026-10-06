// Package linkcheck is the deployment link check (spec 4.8): a short
// burst of RC1 P probes to a peer, whose app answers with an RC1 Q
// summary, to confirm a usable RF link before the race relies on it.
package linkcheck

import (
	"strings"
	"time"
)

// Verdicts.
const (
	Pass     = "PASS"
	Marginal = "MARGINAL"
	Fail     = "FAIL"
)

// Audio-level advice thresholds (dBFS). Advice only, not the verdict.
const (
	hotLevel = -6
	lowLevel = -40
)

// passRTT is the slowest median round trip that still passes.
const passRTT = 20 * time.Second

// Measure is what the prober learned from one run.
type Measure struct {
	N         int  // probes sent
	Uplink    int  // probes the responder heard (from its reply)
	RoundTrip int  // probes ACKed (each one crossed the link both ways)
	Reply     bool // the responder's reply arrived
	MedianRTT *time.Duration
}

// Verdict grades a run (spec 4.8.4). An ACKed probe was necessarily
// heard, so uplink is at least the round-trip count even if the reply
// was lost.
func Verdict(m Measure) string {
	up := max(m.Uplink, m.RoundTrip)
	if m.N <= 0 {
		return Fail
	}
	if up >= m.N-1 && m.RoundTrip >= m.N-1 && m.Reply && m.MedianRTT != nil && *m.MedianRTT <= passRTT {
		return Pass
	}
	// "At least half in each direction": uplink is probes heard out of
	// those sent; downlink is ACKs received out of probes heard (an ACK
	// can only come back for a probe that got there).
	if 2*up >= m.N && up > 0 && 2*m.RoundTrip >= up {
		return Marginal
	}
	return Fail
}

// ResponderVerdict grades a run from the responder's side: it knows
// what it heard and whether its reply was ACKed (which proves the link
// back), but not the prober's round trips.
func ResponderVerdict(total, heard int, replyAcked bool) string {
	switch {
	case total > 0 && heard >= total-1 && replyAcked:
		return Pass
	case total > 0 && 2*heard >= total:
		return Marginal
	default:
		return Fail
	}
}

// Advice explains a result in the operator's terms.
func Advice(verdict string, remote, local *int) string {
	var tips []string
	switch verdict {
	case Marginal:
		tips = append(tips, "usable, but expect retries and gap requests: try a higher antenna, a relay station or a digipeater path")
	case Fail:
		tips = append(tips, "not usable: check both stations are on the same frequency and channel, the HQ callsign, the antenna, then try a relay or a digipeater path")
	}
	for _, l := range []struct {
		where string
		lvl   *int
	}{{"at the far station", remote}, {"here", local}} {
		switch {
		case l.lvl == nil:
		case *l.lvl > hotLevel:
			tips = append(tips, "receive audio "+l.where+" is too hot (turn the radio's volume or the input gain down)")
		case *l.lvl < lowLevel:
			tips = append(tips, "receive audio "+l.where+" is very low (turn the volume or input gain up)")
		}
	}
	return strings.Join(tips, "; ")
}
