package hq

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// CheckpointHealth is HQ's view of one checkpoint for the health panel
// (spec 4.2.4, 4.4): its status, the batches HQ knows it is missing,
// those it has stopped requesting, and whether its traffic comes from
// the expected station. Bibs inside a missing batch are unknown to HQ.
type CheckpointHealth struct {
	store.CheckpointStatus
	Name           string
	CourseOrder    int
	ExpectedCall   string
	Defined        bool // on HQ's checkpoint list
	SenderMismatch bool // last heard from a call other than ExpectedCall
	Missing        []uint32
	GivenUp        []uint32
}

// Health reports every checkpoint on HQ's list (heard or not) and every
// code HQ has heard from, in course order, then unknown codes by code.
func (e *Engine) Health(ctx context.Context) ([]CheckpointHealth, error) {
	statuses, err := e.store.ListStatuses(ctx)
	if err != nil {
		return nil, err
	}
	cps, err := e.store.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	byCode := map[string]*CheckpointHealth{}
	for _, c := range cps {
		byCode[c.Code] = &CheckpointHealth{
			CheckpointStatus: store.CheckpointStatus{CPCode: c.Code},
			Name:             c.Name, CourseOrder: c.CourseOrder, ExpectedCall: c.ExpectedCall, Defined: true,
		}
	}
	for _, st := range statuses {
		h := byCode[st.CPCode]
		if h == nil {
			h = &CheckpointHealth{}
			byCode[st.CPCode] = h
		}
		h.CheckpointStatus = st
		missing, err := e.store.MissingSeqs(ctx, st.CPCode, wire.MaxGapSeqs)
		if err != nil {
			return nil, err
		}
		h.Missing, h.GivenUp = missing, e.unrecoverable(st.CPCode)
	}
	out := make([]CheckpointHealth, 0, len(byCode))
	for _, h := range byCode {
		h.SenderMismatch = h.ExpectedCall != "" && h.LastSourceCall != "" &&
			!strings.EqualFold(h.ExpectedCall, h.LastSourceCall)
		out = append(out, *h)
	}
	slices.SortFunc(out, func(a, b CheckpointHealth) int {
		// Defined checkpoints first, in course order; then heard-only codes.
		if a.Defined != b.Defined {
			if a.Defined {
				return -1
			}
			return 1
		}
		return cmp.Or(cmp.Compare(a.CourseOrder, b.CourseOrder), cmp.Compare(a.CPCode, b.CPCode))
	})
	return out, nil
}

// Rearm is the operator's "Re-request": every batch cp is missing,
// including ones HQ gave up on, becomes requestable on the next tick
// with a fresh set of attempts. At most once per checkpoint per
// rearmMinInterval (ErrTooSoon). Returns how many were re-armed.
func (e *Engine) Rearm(ctx context.Context, cfg store.Settings, cp string) (int, error) {
	statuses, err := e.store.ListStatuses(ctx)
	if err != nil {
		return 0, err
	}
	if !slices.ContainsFunc(statuses, func(st store.CheckpointStatus) bool { return st.CPCode == cp }) {
		return 0, store.ErrNotFound
	}
	missing, err := e.store.MissingSeqs(ctx, cp, wire.MaxGapSeqs)
	if err != nil {
		return 0, err
	}
	now := e.now()
	grace := time.Duration(cfg.GapGraceSec) * time.Second
	e.mu.Lock()
	if last, ok := e.lastRearm[cp]; ok && now.Sub(last) < rearmMinInterval {
		e.mu.Unlock()
		return 0, ErrTooSoon
	}
	e.lastRearm[cp] = now
	e.mu.Unlock()

	gs := e.reconcile(cp, missing, now)
	if gs == nil {
		return 0, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, g := range gs.seqs {
		*g = seqGap{firstSeen: now.Add(-grace)}
	}
	gs.nextReq = time.Time{}
	e.log.Info("hq: operator re-requested missing batches", "cp", cp, "batches", len(gs.seqs))
	return len(gs.seqs), nil
}
