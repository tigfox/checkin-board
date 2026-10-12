// Package bench runs a scripted race across real checkin-board nodes
// (spec 9, phase 12 g): it logs bibs on each checkpoint's keypad API on
// a schedule, then reads HQ's results to check that every passage
// arrived exactly once, and how long it took. It only uses the app's own
// web API, as an operator's browser does.
package bench

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"time"
)

const (
	// MaxBib is the highest bib the keypad accepts.
	MaxBib = 9999
	// paceSpread: each runner's pace is the field's ±15%.
	paceSpread = 0.15
)

// PlanConfig describes the scripted race.
type PlanConfig struct {
	// Codes are the checkpoints in course order, one per checkpoint node.
	Codes []string
	// Runners take bibs FirstBib .. FirstBib+Runners-1.
	Runners, FirstBib int
	// Duration is when the slowest runner reaches the last checkpoint.
	Duration time.Duration
	Seed     uint64
}

// Event is one passage to log: checkpoint Code (node index Node) logs
// Bib at At after the start.
type Event struct {
	Node int
	Code string
	Bib  int
	At   time.Duration
}

// Plan is the schedule, in time order.
type Plan struct {
	Events []Event
}

// LastBib is the plan's highest bib.
func (c PlanConfig) LastBib() int { return c.FirstBib + c.Runners - 1 }

// Validate checks the plan's inputs.
func (c PlanConfig) Validate() error {
	switch {
	case len(c.Codes) == 0:
		return errors.New("bench: no checkpoints")
	case c.Runners < 1:
		return errors.New("bench: runners must be at least 1")
	case c.FirstBib < 1 || c.LastBib() > MaxBib:
		return fmt.Errorf("bench: bibs %d-%d are outside 1-%d", c.FirstBib, c.LastBib(), MaxBib)
	case c.Duration <= 0:
		return errors.New("bench: duration must be positive")
	}
	seen := map[string]bool{}
	for _, code := range c.Codes {
		if code == "" || seen[code] {
			return fmt.Errorf("bench: checkpoint codes must be set and unique, got %v", c.Codes)
		}
		seen[code] = true
	}
	return nil
}

// NewPlan builds a mass-start race: every runner starts at once and
// keeps a steady pace (the field's ±15%), so the first checkpoint gets
// the busiest burst, as on race morning. The same seed gives the same
// plan.
func NewPlan(c PlanConfig) (Plan, error) {
	if err := c.Validate(); err != nil {
		return Plan{}, err
	}
	rng := rand.New(rand.NewPCG(c.Seed, c.Seed^0x9e3779b97f4a7c15))
	n := len(c.Codes)
	slowest := 1 + paceSpread
	var events []Event
	for r := range c.Runners {
		pace := 1 + paceSpread*(2*rng.Float64()-1)
		for i, code := range c.Codes {
			frac := float64(i+1) / float64(n) * pace / slowest
			events = append(events, Event{Node: i, Code: code, Bib: c.FirstBib + r, At: time.Duration(frac * float64(c.Duration))})
		}
	}
	sort.SliceStable(events, func(a, b int) bool { return events[a].At < events[b].At })
	return Plan{Events: events}, nil
}
