package bench

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"checkin-board/internal/store"
)

const (
	logAttempts = 4
	retryWait   = 2 * time.Second
	// gracePolls: HQ is read this many more times after every passage
	// arrived, so a late duplicate (a retransmitted batch) still shows.
	gracePolls = 2
)

// Config is one bench run.
type Config struct {
	HQ          string   // HQ node's base URL
	Checkpoints []string // checkpoint nodes' base URLs, in course order
	Password    string   // admin password (the same on every node)
	Runners     int
	FirstBib    int
	Duration    time.Duration // race length (see PlanConfig)
	Settle      time.Duration // how long to wait for HQ after the last bib
	PollEvery   time.Duration // HQ results polling
	Seed        uint64
	Out         io.Writer // progress; nil: none
	// Now and Sleep are the clock (nil: real time).
	Now   func() time.Time
	Sleep func(context.Context, time.Duration) error
}

var (
	// ErrSetup means the nodes aren't ready for a bench; nothing was
	// logged and no node was changed.
	ErrSetup = errors.New("bench: setup")
	// ErrInterrupted means the run stopped after logging began: the
	// nodes hold bench entries and need a race reset.
	ErrInterrupted = errors.New("bench: interrupted after logging began; reset the race on every node")
	// ErrIncomplete means HQ couldn't be read at the end: the report is
	// from the last successful read.
	ErrIncomplete = errors.New("bench: HQ unreadable at the end; the report may be stale")
)

// Run logs the scripted race on the checkpoints and waits for HQ. It
// changes nothing on any node unless every node has the right role with
// the race running, each checkpoint has an HQ callsign, and none of the
// bench's bibs is in use. After logging began, the report comes back
// even with an error.
func Run(ctx context.Context, c Config) (Report, error) {
	c = c.withDefaults()
	if len(c.Checkpoints) == 0 {
		return Report{}, fmt.Errorf("%w: no checkpoint nodes", ErrSetup)
	}
	pc := PlanConfig{Runners: c.Runners, FirstBib: c.FirstBib, Duration: c.Duration, Seed: c.Seed}
	for i := range c.Checkpoints {
		pc.Codes = append(pc.Codes, fmt.Sprintf("CP%d", i)) // placeholders until the nodes say
	}
	if err := pc.Validate(); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrSetup, err)
	}
	hq := NewNode(c.HQ)
	cps := make([]*Node, len(c.Checkpoints))
	for i, u := range c.Checkpoints {
		cps[i] = NewNode(u)
	}
	codes, err := c.check(ctx, hq, cps)
	if err != nil {
		return Report{}, err
	}
	pc.Codes = codes
	plan, err := NewPlan(pc)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrSetup, err)
	}
	// Every check passed: from here on the nodes change.
	for _, n := range cps {
		if err := n.SyncClock(ctx, c.Now()); err != nil {
			return Report{}, fmt.Errorf("%w: %s: clock sync: %v", ErrSetup, n.Base, err)
		}
	}
	c.printf("bench: %d runners (bibs %d-%d) through %v over %s: %d passages\n",
		c.Runners, pc.FirstBib, pc.LastBib(), codes, c.Duration, len(plan.Events))

	logged, gaveUp, err := c.logAll(ctx, cps, plan)
	if err != nil {
		if len(logged) == 0 && len(gaveUp) == 0 {
			return Report{}, err
		}
		rep, _ := c.read(context.WithoutCancel(ctx), hq, logged, gaveUp, pc)
		return rep, fmt.Errorf("%w (%v)", ErrInterrupted, err)
	}
	return c.await(ctx, hq, logged, gaveUp, pc)
}

func (c Config) withDefaults() Config {
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Sleep == nil {
		c.Sleep = sleepCtx
	}
	if c.PollEvery <= 0 {
		c.PollEvery = 10 * time.Second
	}
	if c.Out == nil {
		c.Out = io.Discard
	}
	return c
}

func (c Config) printf(format string, args ...any) { fmt.Fprintf(c.Out, format, args...) }

func (c Config) inRange(bib int) bool { return bib >= c.FirstBib && bib < c.FirstBib+c.Runners }

// check logs in everywhere and checks each node is ready, changing
// nothing. It returns the checkpoints' codes in course order.
func (c Config) check(ctx context.Context, hq *Node, cps []*Node) ([]string, error) {
	setup := func(n *Node, format string, args ...any) error {
		return fmt.Errorf("%w: %s: %s", ErrSetup, n.Base, fmt.Sprintf(format, args...))
	}
	ready := func(n *Node, role string) (Station, error) {
		if err := n.Login(ctx, c.Password); err != nil {
			return Station{}, setup(n, "login: %v", err)
		}
		st, err := n.Station(ctx)
		switch {
		case err != nil:
			return st, setup(n, "%v", err)
		case st.Role != role:
			return st, setup(n, "role %q, want %q", st.Role, role)
		case st.RaceState != store.RaceActive:
			return st, setup(n, "race state %q; start the race (checkpoints: open the checkpoint) first", st.RaceState)
		}
		return st, nil
	}
	if _, err := ready(hq, store.RoleHQ); err != nil {
		return nil, err
	}
	codes := make([]string, len(cps))
	seen := map[string]bool{}
	for i, n := range cps {
		st, err := ready(n, store.RoleCheckpoint)
		if err != nil {
			return nil, err
		}
		if len(st.LocalCodes) == 0 || seen[st.LocalCodes[0]] {
			return nil, setup(n, "checkpoint code missing or used by another node (%v)", st.LocalCodes)
		}
		codes[i] = st.LocalCodes[0]
		seen[codes[i]] = true
		if call, err := n.HQCall(ctx); err != nil || call == "" {
			return nil, setup(n, "no HQ callsign set (%v)", err)
		}
		bibs, err := n.RecentBibs(ctx)
		if err != nil {
			return nil, setup(n, "entries: %v", err)
		}
		for _, b := range bibs {
			if c.inRange(b) {
				return nil, setup(n, "already has bib %d: reset the race, or pick other bibs (-first-bib)", b)
			}
		}
	}
	results, err := hq.Results(ctx)
	if err != nil {
		return nil, setup(hq, "results: %v", err)
	}
	for _, r := range results {
		if c.inRange(r.Bib) {
			return nil, setup(hq, "HQ already has bib %d at %s: reset the race, or pick other bibs (-first-bib)", r.Bib, r.Code)
		}
	}
	return codes, nil
}

// logAll logs every event at its time, one worker per checkpoint node so
// a slow node delays only its own bibs. A bib that can't be logged is
// reported and left out of the expected count (gaveUp): the bench
// measures delivery, not the keypad.
func (c Config) logAll(ctx context.Context, cps []*Node, plan Plan) ([]Logged, []Attempt, error) {
	start := c.Now()
	// The run's start makes request ids unique across runs: the node's
	// retry cache outlives a race reset.
	runID := start.UnixNano()
	perNode := make([][]Event, len(cps))
	for _, e := range plan.Events {
		perNode[e.Node] = append(perNode[e.Node], e)
	}
	var (
		mu       sync.Mutex
		logged   []Logged
		gaveUp   []Attempt
		firstErr error
		wg       sync.WaitGroup
	)
	total := len(plan.Events)
	for i, events := range perNode {
		wg.Go(func() {
			for _, e := range events {
				if wait := start.Add(e.At).Sub(c.Now()); wait > 0 {
					if err := c.Sleep(ctx, wait); err != nil {
						mu.Lock()
						firstErr = cmpOr(firstErr, err)
						mu.Unlock()
						return
					}
				}
				id := fmt.Sprintf("bench-%d-%d-%s-%d", runID, c.Seed, e.Code, e.Bib)
				in, err := logWithRetry(ctx, c, cps[i], e.Bib, id)
				mu.Lock()
				if err != nil {
					gaveUp = append(gaveUp, Attempt{e.Code, e.Bib})
					c.printf("bench: %s bib %d not logged: %v\n", e.Code, e.Bib, err)
				} else {
					logged = append(logged, Logged{Code: e.Code, Bib: e.Bib, TimeIn: in})
				}
				if done := len(logged) + len(gaveUp); done%max(total/10, 1) == 0 || done == total {
					c.printf("bench: logged %d/%d (%s)\n", len(logged), total, c.Now().Sub(start).Round(time.Second))
				}
				mu.Unlock()
				if ctx.Err() != nil {
					return
				}
			}
		})
	}
	wg.Wait()
	if err := cmpOr(firstErr, ctx.Err()); err != nil {
		return logged, gaveUp, err
	}
	if len(gaveUp) > 0 {
		c.printf("bench: %d bibs got no clear answer (left out; if HQ has them they show as unconfirmed)\n", len(gaveUp))
	}
	if len(logged) == 0 {
		return nil, gaveUp, errors.New("bench: no bib could be logged")
	}
	return logged, gaveUp, nil
}

func cmpOr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// logWithRetry retries with the same request id, so the node logs the
// bib at most once however often it's sent.
func logWithRetry(ctx context.Context, c Config, n *Node, bib int, id string) (time.Time, error) {
	var err error
	for attempt := range logAttempts {
		if attempt > 0 {
			if serr := c.Sleep(ctx, retryWait); serr != nil {
				return time.Time{}, serr
			}
		}
		var in time.Time
		if in, err = n.LogBib(ctx, bib, id); err == nil {
			return in, nil
		}
		if !Retryable(err) || ctx.Err() != nil {
			break
		}
	}
	return time.Time{}, err
}

func (c Config) read(ctx context.Context, hq *Node, logged []Logged, gaveUp []Attempt, pc PlanConfig) (Report, error) {
	results, err := hq.Results(ctx)
	if err != nil {
		return Report{}, err
	}
	return Analyze(logged, gaveUp, results, pc.FirstBib, pc.LastBib()), nil
}

// await polls HQ until every logged passage is there (then a few more
// times, for late duplicates) or Settle runs out.
func (c Config) await(ctx context.Context, hq *Node, logged []Logged, gaveUp []Attempt, pc PlanConfig) (Report, error) {
	deadline := c.Now().Add(c.Settle)
	var (
		rep     Report
		haveRep bool
		grace   = -1 // polls left after completion; -1: not complete yet
	)
	for {
		r, err := c.read(ctx, hq, logged, gaveUp, pc)
		if err != nil {
			c.printf("bench: HQ results: %v\n", err)
		} else {
			rep, haveRep = r, true
			c.printf("bench: HQ has %d/%d\n", rep.Delivered, rep.Expected)
			if rep.Delivered == rep.Expected && grace < 0 {
				grace = gracePolls
			}
		}
		if grace == 0 {
			return rep, nil
		}
		if !c.Now().Before(deadline) && grace < 0 {
			if err != nil {
				if !haveRep {
					return Report{}, fmt.Errorf("%w: %v", ErrIncomplete, err)
				}
				return rep, fmt.Errorf("%w: %v", ErrIncomplete, err)
			}
			return rep, nil
		}
		if grace > 0 {
			grace--
		}
		if err := c.Sleep(ctx, c.PollEvery); err != nil {
			return rep, fmt.Errorf("%w (%v)", ErrInterrupted, err)
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
