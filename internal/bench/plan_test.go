package bench

import (
	"reflect"
	"testing"
	"time"
)

func planCfg() PlanConfig {
	return PlanConfig{Codes: []string{"AS1", "AS2"}, Runners: 100, FirstBib: 9001, Duration: 20 * time.Minute, Seed: 7}
}

func TestPlanCoversEveryRunnerAtEveryCheckpoint(t *testing.T) {
	c := planCfg()
	p, err := NewPlan(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Events) != 200 {
		t.Fatalf("events = %d, want 200", len(p.Events))
	}
	seen := map[[2]any]bool{}
	byBib := map[int][]Event{}
	for i, e := range p.Events {
		if i > 0 && e.At < p.Events[i-1].At {
			t.Fatalf("not in time order at %d", i)
		}
		if e.At <= 0 || e.At > c.Duration {
			t.Fatalf("event outside the race: %+v", e)
		}
		if e.Bib < 9001 || e.Bib > 9100 || e.Code != c.Codes[e.Node] {
			t.Fatalf("bad event %+v", e)
		}
		k := [2]any{e.Code, e.Bib}
		if seen[k] {
			t.Fatalf("duplicate %v", k)
		}
		seen[k] = true
		byBib[e.Bib] = append(byBib[e.Bib], e)
	}
	for bib, es := range byBib {
		if es[0].Code != "AS1" || es[1].Code != "AS2" || es[0].At >= es[1].At {
			t.Fatalf("bib %d reaches the checkpoints out of course order: %+v", bib, es)
		}
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	a, _ := NewPlan(planCfg())
	b, _ := NewPlan(planCfg())
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same seed, different plans")
	}
	c := planCfg()
	c.Seed = 8
	d, _ := NewPlan(c)
	if reflect.DeepEqual(a, d) {
		t.Fatal("different seeds, same plan")
	}
}

func TestPlanValidate(t *testing.T) {
	for name, edit := range map[string]func(*PlanConfig){
		"no checkpoints":  func(c *PlanConfig) { c.Codes = nil },
		"duplicate codes": func(c *PlanConfig) { c.Codes = []string{"AS1", "AS1"} },
		"empty code":      func(c *PlanConfig) { c.Codes = []string{""} },
		"no runners":      func(c *PlanConfig) { c.Runners = 0 },
		"bib zero":        func(c *PlanConfig) { c.FirstBib = 0 },
		"bibs past 9999":  func(c *PlanConfig) { c.FirstBib = 9950 },
		"no duration":     func(c *PlanConfig) { c.Duration = 0 },
	} {
		c := planCfg()
		edit(&c)
		if _, err := NewPlan(c); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
