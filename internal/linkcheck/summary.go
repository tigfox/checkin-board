package linkcheck

import (
	"fmt"
	"strings"
	"time"

	"checkin-board/internal/store"
)

// Which node ran a link check.
const (
	SideHQ         = "hq"         // HQ probed the checkpoint
	SideCheckpoint = "checkpoint" // the checkpoint probed HQ
)

// ReadyWindow: Start race warns about links without a PASS this recent.
const ReadyWindow = 2 * time.Hour

// Summary is a checkpoint's latest link-check result, whichever side ran
// it, for HQ's health panel. Level is the receive level at HQ.
type Summary struct {
	At      time.Time `json:"at"`
	Verdict string    `json:"verdict"`
	Heard   int       `json:"heard"`
	Total   int       `json:"total"`
	Level   *int      `json:"level,omitempty"`
	Side    string    `json:"side"`
}

func fromCheck(c store.LinkCheck) (Summary, bool) {
	if c.State != store.LinkCheckDone || c.FinishedAt == nil {
		return Summary{}, false
	}
	return Summary{At: *c.FinishedAt, Verdict: c.Verdict, Heard: max(c.Uplink, c.RoundTrip), Total: c.Count, Level: c.LocalLevel, Side: SideHQ}, true
}

func fromResponse(r store.LinkResponse) Summary {
	heard := len(r.HeardIndices())
	return Summary{At: r.LastHeardAt, Verdict: ResponderVerdict(r.Total, heard, r.ReplyAckedAt != nil),
		Heard: heard, Total: r.Total, Level: r.Level, Side: SideCheckpoint}
}

// Latest returns each checkpoint's newest result at HQ, keyed by code:
// HQ's own runs (matched by the checkpoint's expected callsign) and the
// runs checkpoints made towards HQ (by the code in their probes).
func Latest(cps []store.Checkpoint, checks []store.LinkCheck, responses []store.LinkResponse) map[string]Summary {
	byCall := map[string]string{}
	for _, c := range cps {
		if c.ExpectedCall != "" {
			byCall[strings.ToUpper(c.ExpectedCall)] = c.Code
		}
	}
	out := map[string]Summary{}
	keep := func(code string, s Summary) {
		if cur, ok := out[code]; !ok || s.At.After(cur.At) {
			out[code] = s
		}
	}
	for _, c := range checks {
		code, known := byCall[strings.ToUpper(c.PeerCall)]
		if s, ok := fromCheck(c); ok && known {
			keep(code, s)
		}
	}
	for _, r := range responses {
		keep(r.ProberCode, fromResponse(r))
	}
	return out
}

// Readiness lists links without a PASS in the last ReadyWindow, for the
// Start race confirmation (a warning; it never blocks).
func Readiness(cfg store.Settings, cps []store.Checkpoint, checks []store.LinkCheck, responses []store.LinkResponse, now time.Time) []string {
	recent := func(s Summary) bool { return s.Verdict == Pass && now.Sub(s.At) <= ReadyWindow }
	switch cfg.Role {
	case store.RoleCheckpoint:
		for _, c := range checks {
			if s, ok := fromCheck(c); ok && strings.EqualFold(c.PeerCall, cfg.HQCall) && recent(s) {
				return nil
			}
		}
		for _, r := range responses {
			if strings.EqualFold(r.PeerCall, cfg.HQCall) && recent(fromResponse(r)) {
				return nil
			}
		}
		return []string{"No passing link check to HQ in the last 2 hours: run one from Admin → Link check."}
	case store.RoleHQ:
		latest := Latest(cps, checks, responses)
		pass := map[string]bool{}
		// Any recent PASS counts, not only the newest result.
		for _, c := range checks {
			for _, cp := range cps {
				if s, ok := fromCheck(c); ok && strings.EqualFold(c.PeerCall, cp.ExpectedCall) && recent(s) {
					pass[cp.Code] = true
				}
			}
		}
		for _, r := range responses {
			if recent(fromResponse(r)) {
				pass[r.ProberCode] = true
			}
		}
		var warn []string
		for _, cp := range cps {
			if pass[cp.Code] {
				continue
			}
			last := "never checked"
			if s, ok := latest[cp.Code]; ok {
				last = fmt.Sprintf("last %s %s ago", s.Verdict, now.Sub(s.At).Round(time.Minute))
			}
			warn = append(warn, fmt.Sprintf("%s (%s): no passing link check in the last 2 hours (%s).", cp.Code, cp.Name, last))
		}
		return warn
	}
	return nil
}
