package bench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/nodeapi"
	"checkin-board/internal/store"
)

// fakeNode is a scripted node API: enough of checkin-board's for Run.
type fakeNode struct {
	t             *testing.T
	role          string
	state         string
	code          string
	hqCall        string
	mu            sync.Mutex
	conflictFirst int               // 409s to answer for each new request_id
	conflict      map[string]int    // request_id → 409s answered
	ids           map[string]string // request_id → bib it logged
	entries       []Result          // logged here (checkpoint) / received (HQ)
	extra         []Result          // HQ: extra rows, shown from poll showExtraAt
	showExtraAt   int
	forward       *fakeNode // checkpoint: HQ to deliver each entry to at once
	polls         int
	failCSV       func(poll int) bool
	writes        int // POSTs that change the node (entries, clock)
	srv           *httptest.Server
}

func newFake(t *testing.T, role, code string) *fakeNode {
	f := &fakeNode{t: t, role: role, state: store.RaceActive, code: code, hqCall: "HQ-1",
		conflict: map[string]int{}, ids: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	switch r.Method + " " + r.URL.Path {
	case "POST /api/login":
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["password"] != "pw" {
			http.Error(w, `{"error":"wrong password"}`, http.StatusUnauthorized)
		}
	case "GET /api/station":
		reply(Station{Role: f.role, RaceState: f.state, LocalCodes: []string{f.code}})
	case "GET /api/admin/settings":
		reply(map[string]string{"hq_call": f.hqCall})
	case "GET /api/entries":
		var es []map[string]int
		for _, e := range f.entries {
			es = append(es, map[string]int{"Bib": e.Bib})
		}
		reply(map[string]any{"entries": es})
	case "POST /api/clock/sync":
		f.writes++
	case "POST /api/entries":
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		id := b["request_id"]
		if f.conflict[id] < f.conflictFirst {
			f.conflict[id]++
			http.Error(w, `{"code":"in_progress"}`, http.StatusConflict)
			return
		}
		in := t0.Add(time.Duration(len(f.entries)) * time.Second)
		if prev, ok := f.ids[id]; ok {
			if prev != b["bib"] {
				f.t.Errorf("request id %s reused for bib %s (was %s)", id, b["bib"], prev)
			}
			reply(map[string]time.Time{"time_in": in})
			return
		}
		f.writes++
		f.ids[id] = b["bib"]
		var bib int
		fmt.Sscan(b["bib"], &bib)
		e := Result{Code: f.code, Bib: bib, TimeIn: in, ReceivedAt: in.Add(time.Second), Count: 1}
		f.entries = append(f.entries, e)
		if f.forward != nil {
			f.forward.mu.Lock()
			f.forward.entries = append(f.forward.entries, e)
			if len(f.forward.extra) == 0 {
				f.forward.extra = []Result{e} // its late duplicate, if shown
			}
			f.forward.mu.Unlock()
		}
		w.WriteHeader(http.StatusCreated)
		reply(map[string]time.Time{"time_in": in})
	case "GET /api/admin/export.csv":
		f.polls++
		if f.failCSV != nil && f.failCSV(f.polls) {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		var b strings.Builder
		b.WriteString("bib,category,checkpoint,checkpoint_name,time_in,source,received_at,count\n")
		rows := f.entries
		if f.showExtraAt > 0 && f.polls >= f.showExtraAt {
			rows = append(append([]Result{}, rows...), f.extra...)
		}
		for _, e := range rows {
			fmt.Fprintf(&b, "%d,,%s,,%s,X,%s,%d\n", e.Bib, e.Code, e.TimeIn.Format(time.RFC3339), e.ReceivedAt.Format(time.RFC3339), e.Count)
		}
		_, _ = w.Write([]byte(b.String()))
	default:
		http.NotFound(w, r)
	}
}

// relay copies the checkpoint's entries to HQ, as a perfect radio would.
type relay struct {
	hq  *fakeNode
	cps []*fakeNode
}

func (r relay) sync() {
	var all []Result
	for _, cp := range r.cps {
		cp.mu.Lock()
		all = append(all, cp.entries...)
		cp.mu.Unlock()
	}
	r.hq.mu.Lock()
	r.hq.entries = all
	r.hq.mu.Unlock()
}

func fastConfig(hq *fakeNode, cps ...*fakeNode) Config {
	c := Config{HQ: hq.srv.URL, Password: "pw", Runners: 3, FirstBib: 9001, Duration: time.Second,
		Settle: time.Minute, PollEvery: time.Second, Seed: 1}
	for _, cp := range cps {
		c.Checkpoints = append(c.Checkpoints, cp.srv.URL)
	}
	return c
}

// fakeClock: Sleep advances it at once, and also relays the radio.
func fakeClock(c *Config, onSleep func()) {
	now := t0
	var mu sync.Mutex
	c.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	c.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		now = now.Add(d)
		mu.Unlock()
		if onSleep != nil {
			onSleep()
		}
		return ctx.Err()
	}
}

func TestRunRetriesConflictWithTheSameID(t *testing.T) {
	hq, cp := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	c := fastConfig(hq, cp)
	fakeClock(&c, relay{hq, []*fakeNode{cp}}.sync)
	// Every first attempt is answered "still saving" twice.
	cp.conflictFirst = 2
	rep, err := Run(context.Background(), c)
	if err != nil || !rep.OK() || rep.Expected != 3 {
		t.Fatalf("rep = %+v, err = %v", rep, err)
	}
	if len(cp.entries) != 3 {
		t.Fatalf("checkpoint logged %d entries, want 3", len(cp.entries))
	}
}

func TestRunCatchesLateDuplicate(t *testing.T) {
	hq, cp := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	c := fastConfig(hq, cp)
	fakeClock(&c, relay{hq, []*fakeNode{cp}}.sync)
	rep, err := Run(context.Background(), c)
	if err != nil || !rep.OK() {
		t.Fatalf("clean run: rep = %+v, err = %v", rep, err)
	}
	// Same again, delivered at once, but HQ shows a second row for one
	// passage only after it first had them all (poll 1 is the setup
	// check, poll 2 the first complete one).
	hq2, cp2 := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	cp2.forward, hq2.showExtraAt = hq2, 3
	c = fastConfig(hq2, cp2)
	fakeClock(&c, nil)
	rep, err = Run(context.Background(), c)
	if err != nil || rep.OK() || len(rep.Duplicates) != 1 {
		t.Fatalf("late duplicate missed: rep = %+v, err = %v", rep, err)
	}
}

func TestRunInterruptedKeepsReport(t *testing.T) {
	hq, cp := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	c := fastConfig(hq, cp)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sleeps := 0
	fakeClock(&c, func() {
		relay{hq, []*fakeNode{cp}}.sync()
		if sleeps++; sleeps == 2 {
			cancel()
		}
	})
	rep, err := Run(ctx, c)
	if !errors.Is(err, ErrInterrupted) || rep.Expected == 0 {
		t.Fatalf("rep = %+v, err = %v", rep, err)
	}
}

func TestRunFinalPollError(t *testing.T) {
	hq, cp := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	c := fastConfig(hq, cp)
	c.Settle = 0
	fakeClock(&c, nil)
	hq.failCSV = func(poll int) bool { return poll > 1 } // the setup check's read works
	rep, err := Run(context.Background(), c)
	if !errors.Is(err, ErrIncomplete) || rep.OK() {
		t.Fatalf("rep = %+v, err = %v", rep, err)
	}
}

// Refusals change nothing on any node: no clock sync, no entries.
func TestRunRefusesWithoutWriting(t *testing.T) {
	for name, edit := range map[string]func(hq, cp1, cp2 *fakeNode){
		"HQ not started":      func(hq, _, _ *fakeNode) { hq.state = store.RaceSetup },
		"second cp not open":  func(_, _, cp2 *fakeNode) { cp2.state = store.RaceSetup },
		"no HQ callsign":      func(_, _, cp2 *fakeNode) { cp2.hqCall = "" },
		"same code twice":     func(_, cp1, cp2 *fakeNode) { cp2.code = cp1.code },
		"bib already at a cp": func(_, _, cp2 *fakeNode) { cp2.entries = []Result{{Code: "AS2", Bib: 9002}} },
		"bib already at HQ":   func(hq, _, _ *fakeNode) { hq.entries = []Result{{Code: "AS1", Bib: 9003, TimeIn: t0, ReceivedAt: t0}} },
		"wrong role for a cp": func(_, _, cp2 *fakeNode) { cp2.role = store.RoleHQ },
	} {
		hq, cp1, cp2 := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1"), newFake(t, store.RoleCheckpoint, "AS2")
		edit(hq, cp1, cp2)
		c := fastConfig(hq, cp1, cp2)
		fakeClock(&c, nil)
		if _, err := Run(context.Background(), c); !errors.Is(err, ErrSetup) {
			t.Errorf("%s: err = %v, want a setup error", name, err)
		}
		if w := cp1.writes + cp2.writes + hq.writes; w != 0 {
			t.Errorf("%s: %d writes before refusing", name, w)
		}
	}
	// A bad plan is refused before any node is contacted.
	hq, cp := newFake(t, store.RoleHQ, "START"), newFake(t, store.RoleCheckpoint, "AS1")
	c := fastConfig(hq, cp)
	c.FirstBib = 9999
	if _, err := Run(context.Background(), c); !errors.Is(err, ErrSetup) || hq.polls != 0 {
		t.Fatalf("bad plan: err = %v, HQ polled %d", err, hq.polls)
	}
}

func TestRetryable(t *testing.T) {
	for err, want := range map[error]bool{
		&nodeapi.APIError{Status: 500}: true,
		&nodeapi.APIError{Status: 502}: true,
		&nodeapi.APIError{Status: 409}: true,
		&nodeapi.APIError{Status: 429}: true,
		&nodeapi.APIError{Status: 400}: false,
		&nodeapi.APIError{Status: 422}: false,
		errors.New("conn reset"):       true,
		context.Canceled:               false,
		context.DeadlineExceeded:       false,
	} {
		if got := Retryable(err); got != want {
			t.Errorf("%v: got %v", err, got)
		}
	}
}

func TestParseResultsErrors(t *testing.T) {
	for name, csv := range map[string]string{
		"empty":       "",
		"no count":    "bib,checkpoint,time_in,received_at\n",
		"bad bib":     "bib,checkpoint,time_in,received_at,count\nx,AS1,2026-10-11T13:00:00Z,2026-10-11T13:00:01Z,1\n",
		"bad time":    "bib,checkpoint,time_in,received_at,count\n1,AS1,yesterday,2026-10-11T13:00:01Z,1\n",
		"ragged rows": "bib,checkpoint,time_in,received_at,count\n1,AS1\n",
	} {
		if _, err := ParseResults([]byte(csv)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
