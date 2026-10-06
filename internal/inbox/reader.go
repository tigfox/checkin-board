// Package inbox follows graywolf's message feed and hands race traffic
// to the role's engine (spec 4.6).
//
// All processing goes through one path: CatchUp pages graywolf's
// message list forward from a persisted cursor. graywolf orders that
// feed by updated_at, so it yields new inbound rows and also our own
// outbound rows again whenever their status changes (sent, acked,
// rejected). The SSE stream and a periodic backstop only trigger a
// catch-up; their payloads are hints and are never trusted.
package inbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

const (
	defaultBackstop = 60 * time.Second
	// healthyStream: a stream that stayed up this long resets the backoff.
	healthyStream = 60 * time.Second
	pageLimit     = 100
	// maxRowFailures: after this many failed dispatches of one row it is
	// skipped, so one bad row can't stall the feed forever. The protocol
	// recovers the content: HQ's gap requests re-fetch a missing batch,
	// and an outbound row reappears on its next status change.
	maxRowFailures = 10
	// maxPendingReads bounds the mark-read retry set.
	maxPendingReads = 1000
)

var defaultBackoff = []time.Duration{time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}

// Graywolf is the subset of the graywolf client the reader uses.
type Graywolf interface {
	StreamEventsWithOpen(ctx context.Context, onOpen func(), fn func(graywolf.Event) error) error
	CatchUp(ctx context.Context, p graywolf.ListParams, fn func(graywolf.MessageChange) error) (string, error)
	MarkRead(ctx context.Context, id uint64) error
}

// Store is the subset of the store the reader uses.
type Store interface {
	InboxCursor(ctx context.Context) (string, error)
	InboxSince(ctx context.Context) (time.Time, error)
	SaveInboxCursor(ctx context.Context, cursor string) error
	KnownGWRow(ctx context.Context, gwID uint64) (bool, error)
	RecordGWRow(ctx context.Context, gwID uint64, kind string) (bool, error)
}

// Dispatcher receives race traffic. Both methods must be idempotent:
// HandleOutbound sees a row again on every status change, and a crash
// between HandleInbound and recording the row redelivers it.
//
// Return an error only for problems worth retrying (the row is retried
// on the next catch-up, up to maxRowFailures times). Wrap an error with
// Permanent to skip the row at once; bad input such as undecodable RC1
// text should be recorded by the dispatcher and return nil.
type Dispatcher interface {
	HandleInbound(ctx context.Context, m graywolf.Message) error
	HandleOutbound(ctx context.Context, m graywolf.Message) error
}

// ErrNotReady is returned by a Dispatcher that can't handle race
// traffic yet (e.g. the node has no role). The catch-up stops at that
// row without counting a failure or skipping it, so nothing is lost:
// the row is processed once the node is ready.
var ErrNotReady = errors.New("inbox: node not ready for race traffic")

type permanentError struct{ err error }

func (e permanentError) Error() string { return e.err.Error() }
func (e permanentError) Unwrap() error { return e.err }

// Permanent marks a dispatch error as not worth retrying.
func Permanent(err error) error { return permanentError{err: err} }

// Config configures a Reader.
type Config struct {
	Graywolf   Graywolf
	Store      Store
	Dispatcher Dispatcher
	// Backstop is the catch-up interval when no events arrive. Zero means 60 s.
	Backstop time.Duration
	// Backoff is the reconnect delay ladder. Nil means 1, 2, 5, 10, 30 s.
	Backoff []time.Duration
	Logger  *slog.Logger
}

// Status is a snapshot for the UI's graywolf banner.
type Status struct {
	Connected     bool // event stream accepted by graywolf and open
	AuthFailed    bool
	LastEventAt   time.Time
	LastCatchUpAt time.Time
	StreamError   string // last event-stream failure; cleared when a stream opens
	CatchUpError  string // last catch-up failure; cleared by a successful catch-up
	SkippedRows   int    // rows given up on after repeated dispatch failures
	LastSkipped   string
}

// Reader follows graywolf's message feed.
type Reader struct {
	cfg    Config
	logger *slog.Logger
	kick   chan struct{}

	// Guarded by catchUpMu: one catch-up at a time.
	catchUpMu    sync.Mutex
	failures     map[uint64]int
	pendingReads map[uint64]struct{}

	mu          sync.Mutex
	status      Status
	streamAuth  bool // last stream failure was an auth failure
	catchUpAuth bool // last catch-up failure was an auth failure
}

// New validates cfg and returns a Reader.
func New(cfg Config) (*Reader, error) {
	if cfg.Graywolf == nil || cfg.Store == nil || cfg.Dispatcher == nil {
		return nil, errors.New("inbox: Graywolf, Store and Dispatcher are required")
	}
	if cfg.Backstop <= 0 {
		cfg.Backstop = defaultBackstop
	}
	if len(cfg.Backoff) == 0 {
		cfg.Backoff = defaultBackoff
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Reader{
		cfg:          cfg,
		logger:       logger,
		kick:         make(chan struct{}, 1),
		failures:     map[uint64]int{},
		pendingReads: map[uint64]struct{}{},
	}, nil
}

// Status returns the current connection and catch-up state.
func (r *Reader) Status() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

func (r *Reader) update(fn func(*Status)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.status)
}

// setStreamError records (err != nil) or clears the stream failure.
func (r *Reader) setStreamError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.StreamError, r.streamAuth = errText(err), errors.Is(err, graywolf.ErrAuth)
	r.status.AuthFailed = r.streamAuth || r.catchUpAuth
}

// setCatchUpError records (err != nil) or clears the catch-up failure.
func (r *Reader) setCatchUpError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status.CatchUpError, r.catchUpAuth = errText(err), errors.Is(err, graywolf.ErrAuth)
	r.status.AuthFailed = r.streamAuth || r.catchUpAuth
	if err == nil {
		r.status.LastCatchUpAt = time.Now()
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Kick requests a catch-up soon. Bursts coalesce into one.
func (r *Reader) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Run follows the feed until ctx is done, then returns ctx's error.
func (r *Reader) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { r.worker(ctx) })
	r.streamLoop(ctx)
	wg.Wait()
	return ctx.Err()
}

// worker runs every catch-up: on start, on each kick, and on the backstop.
func (r *Reader) worker(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.Backstop)
	defer ticker.Stop()
	r.Kick()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.kick:
		case <-ticker.C:
		}
		err := r.CatchUp(ctx)
		switch {
		case errors.Is(err, graywolf.ErrCatchUpIncomplete):
			r.Kick() // more backlog: keep draining without waiting for the backstop
		case err != nil && ctx.Err() == nil:
			r.logger.Warn("inbox catch-up failed", "err", err)
		}
	}
}

// streamLoop keeps an SSE connection open, reconnecting with backoff.
func (r *Reader) streamLoop(ctx context.Context) {
	attempt := 0
	for ctx.Err() == nil {
		start := time.Now()
		r.Kick() // catch up on (re)connect: rows may have arrived while down
		err := r.cfg.Graywolf.StreamEventsWithOpen(ctx,
			func() {
				r.update(func(s *Status) { s.Connected = true })
				r.setStreamError(nil)
			},
			func(graywolf.Event) error {
				r.update(func(s *Status) { s.LastEventAt = time.Now() })
				r.Kick()
				return nil
			})
		r.update(func(s *Status) { s.Connected = false })
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			r.setStreamError(err)
		}
		if time.Since(start) >= healthyStream {
			attempt = 0
		}
		delay := r.cfg.Backoff[min(attempt, len(r.cfg.Backoff)-1)]
		attempt++
		r.logger.Info("graywolf event stream ended; reconnecting", "err", err, "in", delay)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// CatchUp pages graywolf's feed from the saved cursor, dispatching race
// traffic, and saves the cursor up to the last fully handled page. It
// is safe to call concurrently with Run; calls are serialized. It
// returns graywolf.ErrCatchUpIncomplete when more backlog remains.
func (r *Reader) CatchUp(ctx context.Context) error {
	r.catchUpMu.Lock()
	defer r.catchUpMu.Unlock()

	r.retryReads(ctx)
	saved, err := r.cfg.Store.InboxCursor(ctx)
	if err != nil {
		err = fmt.Errorf("inbox: load cursor: %w", err)
		r.setCatchUpError(err)
		return err
	}
	params := graywolf.ListParams{Folder: graywolf.FolderAll, Cursor: saved, Limit: pageLimit}
	if saved == "" {
		// A node with no cursor reads from its saved starting point
		// (first run), so it doesn't replay old races from graywolf.
		since, err := r.cfg.Store.InboxSince(ctx)
		if err != nil {
			err = fmt.Errorf("inbox: load starting point: %w", err)
			r.setCatchUpError(err)
			return err
		}
		params.Since = since
	}
	cursor, err := r.cfg.Graywolf.CatchUp(ctx, params, func(ch graywolf.MessageChange) error {
		return r.process(ctx, ch)
	})
	if cursor != saved {
		// Save even if ctx was cancelled mid-catch-up: the progress is real.
		if serr := r.cfg.Store.SaveInboxCursor(context.WithoutCancel(ctx), cursor); serr != nil {
			err = errors.Join(err, fmt.Errorf("inbox: save cursor: %w", serr))
		}
	}
	if err != nil && !errors.Is(err, graywolf.ErrCatchUpIncomplete) {
		r.setCatchUpError(err)
		return err
	}
	r.setCatchUpError(nil)
	return err
}

// process handles one feed row. Non-race rows and tactical threads are
// the operator's traffic and are left alone.
func (r *Reader) process(ctx context.Context, ch graywolf.MessageChange) error {
	m := ch.Message
	if m == nil || m.ThreadKind != graywolf.ThreadKindDM || !wire.IsRaceText(m.Text) {
		return nil
	}
	switch m.Direction {
	case "in":
		return r.processInbound(ctx, *m)
	case "out":
		return r.dispatch(m.ID, func() error { return r.cfg.Dispatcher.HandleOutbound(ctx, *m) })
	default:
		return nil
	}
}

func (r *Reader) processInbound(ctx context.Context, m graywolf.Message) error {
	known, err := r.cfg.Store.KnownGWRow(ctx, m.ID)
	if err != nil {
		return fmt.Errorf("inbox: check row %d: %w", m.ID, err)
	}
	if !known {
		// A row dispatch gave up on is recorded too, so it isn't retried
		// when it reappears in the feed (e.g. after mark-read).
		if err := r.dispatch(m.ID, func() error { return r.cfg.Dispatcher.HandleInbound(ctx, m) }); err != nil {
			return err
		}
		if _, err := r.cfg.Store.RecordGWRow(ctx, m.ID, store.GWRowInbound); err != nil {
			return fmt.Errorf("inbox: record row %d: %w", m.ID, err)
		}
	}
	// Keep the operator's unread count meaningful (spec 4.5).
	if m.Unread {
		r.markRead(ctx, m.ID)
	}
	return nil
}

// dispatch runs fn for row id, counting failures. A Permanent error, or
// the maxRowFailures-th failure, skips the row (returns nil) and
// reports it in Status.
func (r *Reader) dispatch(id uint64, fn func() error) error {
	err := fn()
	if err == nil {
		delete(r.failures, id)
		return nil
	}
	if errors.Is(err, ErrNotReady) {
		return err // hold here; not a failure
	}
	r.failures[id]++
	var perm permanentError
	if !errors.As(err, &perm) && r.failures[id] < maxRowFailures {
		return fmt.Errorf("inbox: row %d: %w", id, err)
	}
	delete(r.failures, id)
	r.logger.Error("inbox: skipping race row after failures", "id", id, "err", err)
	r.update(func(s *Status) {
		s.SkippedRows++
		s.LastSkipped = fmt.Sprintf("row %d: %v", id, err)
	})
	return nil
}

func (r *Reader) markRead(ctx context.Context, id uint64) {
	if err := r.cfg.Graywolf.MarkRead(ctx, id); err != nil {
		r.logger.Warn("inbox: mark read failed; will retry", "id", id, "err", err)
		if len(r.pendingReads) < maxPendingReads {
			r.pendingReads[id] = struct{}{}
		}
		return
	}
	delete(r.pendingReads, id)
}

// retryReads re-attempts mark-reads that failed earlier.
func (r *Reader) retryReads(ctx context.Context) {
	for id := range r.pendingReads {
		r.markRead(ctx, id)
	}
}
