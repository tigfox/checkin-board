package bench

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// Logged is a passage the bench logged on a checkpoint's keypad.
type Logged struct {
	Code   string
	Bib    int
	TimeIn time.Time // the checkpoint's race clock, from its reply
}

// Result is one row of HQ's results export.
type Result struct {
	Code       string
	Bib        int
	TimeIn     time.Time
	ReceivedAt time.Time
	Count      int
}

// Report is the bench's verdict.
type Report struct {
	Expected   int      `json:"expected"`
	Delivered  int      `json:"delivered"`
	Missing    []string `json:"missing,omitempty"`
	Duplicates []string `json:"duplicates,omitempty"`
	Unexpected []string `json:"unexpected,omitempty"`
	// Unconfirmed are passages at HQ for bibs the bench gave up on
	// (no reply): the node logged them after all. They don't fail a run.
	Unconfirmed []string `json:"unconfirmed,omitempty"`
	// NegativeLatency counts passages HQ received "before" the
	// checkpoint logged them: the nodes' clocks disagree.
	NegativeLatency int        `json:"negative_latency,omitempty"`
	Latency         Stats      `json:"latency"`
	PerCP           []CPReport `json:"per_checkpoint"`
}

// CPReport is one checkpoint's share.
type CPReport struct {
	Code      string `json:"code"`
	Expected  int    `json:"expected"`
	Delivered int    `json:"delivered"`
	Latency   Stats  `json:"latency"`
}

// Stats summarizes delivery latency: HQ's received_at minus the
// checkpoint's time_in. Both are node clocks, to the second.
type Stats struct {
	N   int           `json:"n"`
	P50 time.Duration `json:"p50"`
	P95 time.Duration `json:"p95"`
	Max time.Duration `json:"max"`
}

// OK is true when every passage arrived exactly once and nothing else
// in the bench's bib range did.
func (r Report) OK() bool {
	return r.Expected > 0 && r.Delivered == r.Expected && len(r.Missing) == 0 &&
		len(r.Duplicates) == 0 && len(r.Unexpected) == 0
}

type passage struct {
	code string
	bib  int
	sec  int64
}

func (p passage) String() string {
	return fmt.Sprintf("%s bib %d at %s", p.code, p.bib, time.Unix(p.sec, 0).UTC().Format("15:04:05"))
}

// Attempt is a bib the bench tried to log at a checkpoint.
type Attempt struct {
	Code string
	Bib  int
}

// Analyze compares what the bench logged with HQ's results. Only bibs
// firstBib..lastBib are looked at, so other entries at HQ don't count.
// gaveUp are bibs whose logging got no answer: HQ having them isn't
// unexpected. A result's count is how many times the checkpoint logged
// that passage, so above 1 is a duplicate too.
func Analyze(logged []Logged, gaveUp []Attempt, results []Result, firstBib, lastBib int) Report {
	want := map[passage]bool{}
	for _, l := range logged {
		want[passage{l.Code, l.Bib, l.TimeIn.Unix()}] = true
	}
	uncertain := map[Attempt]bool{}
	for _, a := range gaveUp {
		uncertain[a] = true
	}
	rep := Report{Expected: len(want)}
	got := map[passage]int{}
	lat := map[string][]time.Duration{}
	for _, r := range results {
		if r.Bib < firstBib || r.Bib > lastBib {
			continue
		}
		p := passage{r.Code, r.Bib, r.TimeIn.Unix()}
		switch {
		case want[p]:
		case uncertain[Attempt{r.Code, r.Bib}]:
			rep.Unconfirmed = append(rep.Unconfirmed, p.String())
			continue
		default:
			rep.Unexpected = append(rep.Unexpected, p.String())
			continue
		}
		got[p] += max(r.Count, 1)
		if got[p] == max(r.Count, 1) { // first row for it
			d := r.ReceivedAt.Sub(r.TimeIn)
			if d < 0 {
				rep.NegativeLatency++
			}
			lat[r.Code] = append(lat[r.Code], d)
		}
	}
	perCP := map[string]*CPReport{}
	for p := range want {
		cp := perCP[p.code]
		if cp == nil {
			cp = &CPReport{Code: p.code}
			perCP[p.code] = cp
		}
		cp.Expected++
		switch n := got[p]; {
		case n == 0:
			rep.Missing = append(rep.Missing, p.String())
		case n > 1:
			rep.Duplicates = append(rep.Duplicates, fmt.Sprintf("%s (%d times)", p, n))
			fallthrough
		default:
			cp.Delivered++
			rep.Delivered++
		}
	}
	var all []time.Duration
	for _, code := range sortedKeys(perCP) {
		cp := perCP[code]
		cp.Latency = stats(lat[code])
		all = append(all, lat[code]...)
		rep.PerCP = append(rep.PerCP, *cp)
	}
	rep.Latency = stats(all)
	for _, s := range [][]string{rep.Missing, rep.Duplicates, rep.Unexpected, rep.Unconfirmed} {
		sort.Strings(s)
	}
	return rep
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func stats(ds []time.Duration) Stats {
	if len(ds) == 0 {
		return Stats{}
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	at := func(q float64) time.Duration { return s[int(q*float64(len(s)-1)+0.5)] }
	return Stats{N: len(s), P50: at(0.5), P95: at(0.95), Max: s[len(s)-1]}
}
