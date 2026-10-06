// Package hq is the HQ-side engine (spec 4.2): it ingests checkpoint
// reports and heartbeats from graywolf's inbox, finds batches a
// checkpoint sent (or announced) that HQ lacks, and asks for them with
// "RC1 G" gap requests. Ported from graywolf pkg/race (our own code).
package hq

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/peers"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

const (
	// gapRepeatFloor is the slowest repeat of an unanswered gap request.
	gapRepeatFloor = 300 * time.Second
	// maxGapAttempts is how many times one missing seq is requested
	// before HQ gives up and reports it unrecoverable (about an hour at
	// the default grace). A checkpoint that is merely out of range keeps
	// retrying its unACKed batches on its own.
	maxGapAttempts = 12
	// maxGapRequestsPerTick caps gap-request airtime across checkpoints.
	maxGapRequestsPerTick = 1
	// rearmMinInterval rate-limits the operator's Re-request per checkpoint.
	rearmMinInterval = 60 * time.Second

	tickTimeout  = 10 * time.Second
	writeTimeout = 5 * time.Second
	// defaultTick: gap timers are 30 s and up, so HQ needn't hit the
	// store every second.
	defaultTick = 5 * time.Second
	// sendFailBackoff spaces retries to a checkpoint whose gap request
	// graywolf failed to send, so other checkpoints aren't starved.
	sendFailBackoff = 30 * time.Second
)

// ErrTooSoon: the action was repeated before its minimum interval.
var ErrTooSoon = errors.New("hq: too soon; try again shortly")

// gapBackoff returns the wait before re-requesting a seq requested n
// times: twice the grace after the first request, then max(300 s,
// 2×grace). With the default 90 s grace: at 90 s, 180 s later, then
// every 300 s.
func gapBackoff(n int, grace time.Duration) time.Duration {
	if n <= 1 {
		return 2 * grace
	}
	return max(gapRepeatFloor, 2*grace)
}

// seqGap tracks one missing seq.
type seqGap struct {
	firstSeen time.Time
	lastReq   time.Time
	reqs      int
}

// gapState tracks one checkpoint's open gaps. In memory only: after an
// HQ restart every gap waits out its grace again.
type gapState struct {
	seqs     map[uint32]*seqGap
	nextReq  time.Time // earliest next request to this checkpoint
	requests int
}

// Messages is the subset of the graywolf client the engine uses.
type Messages interface {
	SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error)
}

// Config configures an Engine.
type Config struct {
	Store    *store.Store
	Graywolf Messages
	Clock    *raceclock.Clock
	Now      func() time.Time // nil: time.Now
	Interval time.Duration    // Run's tick interval; zero: 5 s
	// Peers, if set, turns graywolf's own retries off for a checkpoint
	// before HQ first sends it a gap request (spec 3.3).
	Peers  *peers.Ensurer
	Logger *slog.Logger
}

// Engine drives HQ. Tick is called every few seconds by Run; HandleInbound
// and HandleOutbound are called by the inbox reader.
type Engine struct {
	store *store.Store
	gw    Messages
	peers *peers.Ensurer
	clock *raceclock.Clock
	now   func() time.Time
	every time.Duration
	log   *slog.Logger

	gwMaxText atomic.Int32

	mu        sync.Mutex
	gaps      map[string]*gapState // by checkpoint code
	lastRearm map[string]time.Time
	peerErr   string
}

// New validates cfg and returns an Engine.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.Graywolf == nil || cfg.Clock == nil {
		return nil, errors.New("hq: Store, Graywolf and Clock are required")
	}
	e := &Engine{
		store: cfg.Store, gw: cfg.Graywolf, peers: cfg.Peers, clock: cfg.Clock, now: cfg.Now,
		every: cfg.Interval, log: cfg.Logger, gaps: map[string]*gapState{}, lastRearm: map[string]time.Time{},
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.every <= 0 {
		e.every = defaultTick
	}
	if e.log == nil {
		e.log = slog.New(slog.DiscardHandler)
	}
	e.gwMaxText.Store(graywolf.MaxMessageText)
	return e, nil
}

// Reset clears the gap tracker and re-request limits, as after a reset.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	clear(e.gaps)
	clear(e.lastRearm)
	e.peerErr = ""
}

// SetGraywolfMaxText records graywolf's current DM length limit.
func (e *Engine) SetGraywolfMaxText(n int) { e.gwMaxText.Store(int32(n)) }

// Run ticks until ctx is done, doing nothing unless this node is HQ. A
// panic or a hung graywolf costs one tick, not the loop.
func (e *Engine) Run(ctx context.Context) error {
	ticker := time.NewTicker(e.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			e.safeTick(ctx)
		}
	}
}

func (e *Engine) safeTick(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			e.log.Error("hq: tick panicked", "panic", r)
		}
	}()
	tickCtx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()
	cfg, err := e.store.GetSettings(tickCtx)
	if err != nil {
		e.log.Warn("hq: load settings", "err", err)
		return
	}
	if cfg.Role != store.RoleHQ {
		return
	}
	// Gap requests run while racing and after the race is complete, so a
	// checkpoint's final check-in can still recover a batch HQ lost.
	if cfg.RaceState != store.RaceActive && cfg.RaceState != store.RaceComplete {
		return
	}
	if err := e.Tick(tickCtx, cfg); err != nil {
		e.log.Warn("hq: tick", "err", err)
	}
}

// Tick reconciles every checkpoint's gaps with the store and sends at
// most maxGapRequestsPerTick requests. Each missing seq waits
// gap_grace_sec before its first request (the checkpoint's own retries
// may still land), backs off per seq after that, and is abandoned after
// maxGapAttempts. Requests to one checkpoint are spaced by the grace.
//
// Once HQ has a checkpoint list, only listed checkpoints are chased:
// RF source calls are spoofable, and a stream of fake reports for
// phantom codes must not turn into gap-request airtime. With no list
// yet, every heard code is chased.
func (e *Engine) Tick(ctx context.Context, cfg store.Settings) error {
	statuses, err := e.store.ListStatuses(ctx)
	if err != nil {
		return err
	}
	cps, err := e.store.ListCheckpoints(ctx)
	if err != nil {
		return err
	}
	defined := make(map[string]bool, len(cps))
	expected := make(map[string]string, len(cps))
	for _, c := range cps {
		defined[c.Code] = true
		if c.ExpectedCall != "" {
			expected[c.Code] = c.ExpectedCall
		}
	}
	now := e.now()
	grace := time.Duration(cfg.GapGraceSec) * time.Second
	maxLen := min(cfg.MaxTextLen, int(e.gwMaxText.Load()))
	sent := 0
	var errs []error
	for _, st := range statuses {
		if len(defined) > 0 && !defined[st.CPCode] {
			continue
		}
		missing, err := e.store.MissingSeqs(ctx, st.CPCode, wire.MaxGapSeqs)
		if err != nil {
			return err
		}
		gs := e.reconcile(st.CPCode, missing, now)
		to := cmp.Or(expected[st.CPCode], st.LastSourceCall)
		if gs == nil || sent >= maxGapRequestsPerTick || to == "" {
			continue
		}
		chosen, text := e.choose(st.CPCode, gs, now, grace, maxLen)
		if len(chosen) == 0 {
			continue
		}
		if err := e.sendGap(ctx, cfg, to, text); err != nil {
			e.deferCheckpoint(gs, now.Add(sendFailBackoff))
			errs = append(errs, fmt.Errorf("hq: send gap request to %s: %w", to, err))
			continue
		}
		sent++
		e.markRequested(st.CPCode, to, gs, chosen, now, grace)
	}
	return errors.Join(errs...)
}

func (e *Engine) deferCheckpoint(gs *gapState, until time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	gs.nextReq = until
}

func (e *Engine) sendGap(ctx context.Context, cfg store.Settings, to, text string) error {
	e.ensurePeer(ctx, to)
	req := graywolf.SendRequest{To: to, Text: text, Path: cfg.Path}
	if cfg.GWChannel > 0 {
		ch := cfg.GWChannel
		req.Channel = &ch
	}
	msg, err := e.gw.SendMessage(ctx, req)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
	defer cancel()
	if _, err := e.store.RecordGWRow(wctx, msg.ID, store.GWRowGap); err != nil {
		e.log.Warn("hq: record gap request row", "row", msg.ID, "err", err)
	}
	return nil
}

// ensurePeer turns graywolf's retries off for call before sending, so a
// gap request is one frame, not graywolf's four.
func (e *Engine) ensurePeer(ctx context.Context, call string) {
	if e.peers == nil {
		return
	}
	err := e.peers.Ensure(ctx, call)
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	e.mu.Lock()
	changed := msg != e.peerErr
	e.peerErr = msg
	e.mu.Unlock()
	if err != nil && changed {
		e.log.Warn("hq: could not turn off graywolf retries; gap requests will cost more airtime", "to", call, "err", err)
	}
}

// reconcile updates cp's tracker to the current missing set and returns
// it, or nil (forgetting the checkpoint) when nothing is missing.
func (e *Engine) reconcile(cp string, missing []uint32, now time.Time) *gapState {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(missing) == 0 {
		delete(e.gaps, cp)
		return nil
	}
	gs := e.gaps[cp]
	if gs == nil {
		gs = &gapState{seqs: map[uint32]*seqGap{}}
		e.gaps[cp] = gs
	}
	still := make(map[uint32]bool, len(missing))
	for _, seq := range missing {
		still[seq] = true
		if gs.seqs[seq] == nil {
			gs.seqs[seq] = &seqGap{firstSeen: now}
		}
	}
	for seq := range gs.seqs {
		if !still[seq] {
			delete(gs.seqs, seq)
		}
	}
	return gs
}

// choose picks the seqs to request now and the packed request text.
// Eligible seqs are past their grace (first request) or backoff
// (repeats) and under maxGapAttempts. Least-requested, then
// longest-unrequested seqs go first, so a long list rotates rather than
// re-sending its lowest seqs forever; as many as fit are packed.
func (e *Engine) choose(cp string, gs *gapState, now time.Time, grace time.Duration, maxLen int) ([]uint32, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if now.Before(gs.nextReq) {
		return nil, ""
	}
	var eligible []uint32
	for seq, g := range gs.seqs {
		switch {
		case g.reqs >= maxGapAttempts:
		case g.reqs == 0 && now.Sub(g.firstSeen) >= grace:
			eligible = append(eligible, seq)
		case g.reqs > 0 && now.Sub(g.lastReq) >= gapBackoff(g.reqs, grace):
			eligible = append(eligible, seq)
		}
	}
	slices.SortFunc(eligible, func(a, b uint32) int {
		ga, gb := gs.seqs[a], gs.seqs[b]
		return cmp.Or(cmp.Compare(ga.reqs, gb.reqs), ga.lastReq.Compare(gb.lastReq), cmp.Compare(a, b))
	})
	var chosen []uint32
	text := ""
	for _, seq := range eligible {
		t, rest, err := wire.PackGap(cp, append(slices.Clone(chosen), seq), maxLen)
		if err != nil || len(rest) > 0 {
			continue // doesn't fit alongside what's chosen; next request
		}
		chosen, text = append(chosen, seq), t
	}
	return chosen, text
}

// markRequested records a sent request for the chosen seqs.
func (e *Engine) markRequested(cp, to string, gs *gapState, chosen []uint32, now time.Time, grace time.Duration) {
	e.mu.Lock()
	gs.requests++
	gs.nextReq = now.Add(grace)
	var abandoned []uint32
	for _, seq := range chosen {
		g := gs.seqs[seq]
		g.reqs++
		g.lastReq = now
		if g.reqs == maxGapAttempts {
			abandoned = append(abandoned, seq)
		}
	}
	n := gs.requests
	e.mu.Unlock()
	e.log.Info("hq: gap request sent", "cp", cp, "to", to, "seqs", len(chosen), "first", slices.Min(chosen), "request", n)
	if len(abandoned) > 0 {
		e.log.Warn("hq: giving up on missing batches after repeated gap requests",
			"cp", cp, "seqs", abandoned, "attempts", maxGapAttempts)
	}
}

// unrecoverable returns cp's missing seqs HQ has stopped requesting.
func (e *Engine) unrecoverable(cp string) []uint32 {
	e.mu.Lock()
	defer e.mu.Unlock()
	gs := e.gaps[cp]
	if gs == nil {
		return nil
	}
	var out []uint32
	for seq, g := range gs.seqs {
		if g.reqs >= maxGapAttempts {
			out = append(out, seq)
		}
	}
	slices.Sort(out)
	return out
}
