// Package checkpoint is the checkpoint-side engine (spec 4.1): it
// batches logged bibs into RC1 reports, sends them to HQ through
// graywolf's Messages API with a sliding in-flight window and its own
// retry ladder, follows delivery status, answers HQ's gap requests and
// sends heartbeats. Ported from graywolf pkg/race (our own code), with
// graywolf's message rows replacing raw APRS frames and msgids.
package checkpoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	// fastRetransmitAfter is how long after a batch was sent its ACK is
	// assumed lost once HQ confirms a newer one.
	fastRetransmitAfter = 20 * time.Second
	// heartbeatRetry is how soon a failed heartbeat is retried.
	heartbeatRetry = 30 * time.Second
	// pollEvery is how often in-flight batches' graywolf status is read
	// directly, covering status changes the inbox feed can skip (4.6).
	pollEvery = 10 * time.Second
	// pollEarlierCopies is how many earlier copies of each in-flight batch
	// the poll also reads (each retransmit is a new graywolf row, 3.1),
	// bounded so a long dead link doesn't multiply graywolf API calls.
	pollEarlierCopies = 2
	// tickTimeout bounds one tick so a hung graywolf can't freeze the loop.
	tickTimeout = 10 * time.Second
	// writeTimeout bounds the store writes that follow a send. They run
	// on a context detached from the tick, so a tick deadline that
	// expires during a slow send can't leave a sent batch unrecorded.
	writeTimeout = 5 * time.Second
	defaultTick  = time.Second
	// recoverSkew allows for graywolf's clock trailing ours when matching
	// sent rows to a batch (same host normally; small skew otherwise).
	recoverSkew = 2 * time.Minute
)

// retryLadder is the ACK-timeout backoff; after it runs out every retry
// waits retryCeiling. A batch is never abandoned.
var (
	retryLadder  = []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second}
	retryCeiling = 300 * time.Second
)

// retryBackoff returns the wait after the attempt-th transmission.
func retryBackoff(attempt int) time.Duration {
	if attempt >= 1 && attempt <= len(retryLadder) {
		return retryLadder[attempt-1]
	}
	return retryCeiling
}

// fullBatchEntries is a conservative count of entries that always fit
// in one frame of maxLen (worst-case header, group and entry widths).
// Reaching it flushes at once instead of waiting out flush_after_sec.
func fullBatchEntries(maxLen int) int {
	const (
		worstHeader = len(wire.Prefix) + len("R CP1234 4294967295")
		worstGroup  = len(" @2359")
		worstEntry  = len(" -9999/59")
	)
	return max(1, (maxLen-worstHeader-worstGroup)/worstEntry)
}

// Messages is the subset of the graywolf client the engine uses.
type Messages interface {
	SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error)
	ResendMessage(ctx context.Context, id uint64) (graywolf.Message, error)
	GetMessage(ctx context.Context, id uint64) (graywolf.Message, error)
	CatchUp(ctx context.Context, p graywolf.ListParams, fn func(graywolf.MessageChange) error) (string, error)
}

// Config configures an Engine.
type Config struct {
	Store    *store.Store
	Graywolf Messages
	Clock    *raceclock.Clock
	Now      func() time.Time // nil: time.Now
	Interval time.Duration    // Run's tick interval; zero: 1 s
	// Peers, if set, turns graywolf's own retries off for HQ before the
	// first send (spec 3.3). Failure is logged, not fatal: delivery
	// still works, at a higher airtime cost.
	Peers *peers.Ensurer
	// OnCheckedIn, if set, runs when a final check-in completes (e.g. to
	// restore graywolf's per-peer settings). It is retried on later ticks
	// in checked_in (also after a restart) until it returns nil, so it
	// must be idempotent.
	OnCheckedIn func(ctx context.Context) error
	Logger      *slog.Logger
}

// Engine drives the checkpoint side. Tick is called once a second by
// Run; HandleInbound/HandleOutbound are called by the inbox reader.
type Engine struct {
	store       *store.Store
	gw          Messages
	peers       *peers.Ensurer
	onCheckedIn func(ctx context.Context) error
	clock       *raceclock.Clock
	now         func() time.Time
	every       time.Duration
	log         *slog.Logger

	gwMaxText atomic.Int32

	mu            sync.Mutex
	hqCall        string
	lastContact   time.Time
	nextHeartbeat time.Time // zero: send on the first tick
	lastPoll      time.Time
	refusal       Refusal
	peerErr       string    // last Ensure error logged, to log each once
	lastState     string    // race state seen by the previous Tick
	checkInStart  time.Time // when the current final check-in began (local clock)
	heardLocal    time.Time // when HQ was last heard, by this node's clock
	lastRevive    time.Time // last re-expedite of parked batches during check-in
	hookPending   bool      // OnCheckedIn still to run (or retry)
}

// reviveEvery spaces re-expediting parked batches during a check-in.
const reviveEvery = 5 * time.Minute

// Reset clears the engine's in-memory state, as after a node reset.
func (e *Engine) Reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastContact, e.nextHeartbeat, e.lastPoll = time.Time{}, time.Time{}, time.Time{}
	e.refusal, e.peerErr, e.lastState, e.checkInStart = Refusal{}, "", "", time.Time{}
	e.heardLocal, e.lastRevive, e.hookPending = time.Time{}, time.Time{}, false
}

// ensurePeer turns graywolf's retries off for call before sending to it.
func (e *Engine) ensurePeer(ctx context.Context, call string) {
	if e.peers == nil {
		return
	}
	err := e.peers.Ensure(ctx, call)
	e.mu.Lock()
	msg := errText(err)
	changed := msg != e.peerErr
	e.peerErr = msg
	e.mu.Unlock()
	if err != nil && changed {
		e.log.Warn("checkpoint: could not turn off graywolf retries for HQ; race traffic will cost more airtime", "hq", call, "err", err)
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Refusal is graywolf's most recent refusal (HTTP 400) of a send. It
// usually means a settings problem (path, channel, text length) that
// affects every batch, so the admin page shows it as an alert. Batches
// are not parked for it: they stay on the retry ladder and go out once
// the problem is fixed.
type Refusal struct {
	At     time.Time
	Reason string
}

// LastRefusal returns the latest refusal (zero if none since a send
// last succeeded).
func (e *Engine) LastRefusal() Refusal {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refusal
}

func (e *Engine) setRefusal(r Refusal) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refusal = r
}

// detached returns a short context for store writes after a send.
func detached(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), writeTimeout)
}

// New validates cfg and returns an Engine.
func New(cfg Config) (*Engine, error) {
	if cfg.Store == nil || cfg.Graywolf == nil || cfg.Clock == nil {
		return nil, errors.New("checkpoint: Store, Graywolf and Clock are required")
	}
	e := &Engine{store: cfg.Store, gw: cfg.Graywolf, peers: cfg.Peers, onCheckedIn: cfg.OnCheckedIn,
		clock: cfg.Clock, now: cfg.Now, every: cfg.Interval, log: cfg.Logger}
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

// SetGraywolfMaxText records graywolf's current DM length limit (from
// its message preferences); batches never exceed it.
func (e *Engine) SetGraywolfMaxText(n int) { e.gwMaxText.Store(int32(n)) }

func (e *Engine) maxText(cfg store.Settings) int {
	return min(cfg.MaxTextLen, int(e.gwMaxText.Load()))
}

// LastContact is when HQ was last heard from (an ACK or a gap request).
func (e *Engine) LastContact() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastContact
}

func (e *Engine) markContact(at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if at.After(e.lastContact) {
		e.lastContact = at
	}
	e.heardLocal = e.now() // for the check-in test: this node's own clock
}

func (e *Engine) currentHQ() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.hqCall
}

// Run ticks until ctx is done. Each tick reloads settings and does
// nothing unless this node is a checkpoint; a panic or a hung graywolf
// costs one tick, not the loop.
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
			e.log.Error("checkpoint: tick panicked", "panic", r)
		}
	}()
	tickCtx, cancel := context.WithTimeout(ctx, tickTimeout)
	defer cancel()
	cfg, err := e.store.GetSettings(tickCtx)
	if err != nil {
		e.log.Warn("checkpoint: load settings", "err", err)
		return
	}
	if cfg.Role != store.RoleCheckpoint {
		return
	}
	if err := e.Tick(tickCtx, cfg); err != nil {
		e.log.Warn("checkpoint: tick", "err", err)
	}
}

// Tick runs one round: flush the queue into batches, rebind any batch
// whose send outcome was lost, poll in-flight status (before sending,
// so an ACK the feed missed doesn't cost a resend), transmit what is
// due, then heartbeat. Failures are joined and returned after the whole
// round; nothing is lost.
//
// The race state gates it (spec 4.7): nothing goes on air in setup,
// secured (packed up for the trip to HQ) or checked_in. Entering
// checking_in (the final check-in at HQ) makes every unconfirmed batch
// due at once and sends a heartbeat, so HQ can request anything it
// lacks; once all is confirmed and HQ has been heard since the check-in
// began, the node moves to checked_in by itself.
func (e *Engine) Tick(ctx context.Context, cfg store.Settings) error {
	now := e.now()
	e.mu.Lock()
	e.hqCall = cfg.HQCall
	entering := cfg.RaceState != e.lastState
	e.mu.Unlock()

	switch cfg.RaceState {
	case store.RaceActive, store.RaceComplete:
	case store.RaceCheckingIn:
		if entering {
			// Only a successful start counts as entering: a failed one
			// is retried next tick.
			if err := e.beginCheckIn(ctx, now); err != nil {
				return err
			}
		} else if err := e.reviveParked(ctx, now); err != nil {
			return err
		}
	case store.RaceCheckedIn:
		e.setLastState(cfg.RaceState)
		return e.runCheckedInHook(ctx, entering)
	default:
		e.setLastState(cfg.RaceState)
		return nil
	}
	e.setLastState(cfg.RaceState)
	if err := e.flush(ctx, cfg, now); err != nil {
		return fmt.Errorf("checkpoint: flush: %w", err)
	}
	err := errors.Join(e.recoverUnbound(ctx, cfg), e.poll(ctx, cfg, now),
		e.transmit(ctx, cfg, now), e.heartbeat(ctx, cfg, now))
	if cfg.RaceState == store.RaceCheckingIn {
		err = errors.Join(err, e.finishCheckIn(ctx, cfg))
	}
	return err
}

func (e *Engine) setLastState(state string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastState = state
}

func (e *Engine) beginCheckIn(ctx context.Context, now time.Time) error {
	n, err := e.store.ExpediteAll(ctx, now)
	if err != nil {
		return fmt.Errorf("checkpoint: begin check-in: %w", err)
	}
	e.mu.Lock()
	e.checkInStart, e.nextHeartbeat, e.lastRevive = now, time.Time{}, now
	e.mu.Unlock()
	e.log.Info("checkpoint: final check-in started", "batches", n)
	return nil
}

// reviveParked re-expedites during a long check-in, so a batch parked
// mid check-in (e.g. REJected) doesn't block it forever.
func (e *Engine) reviveParked(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	due := now.Sub(e.lastRevive) >= reviveEvery
	if due {
		e.lastRevive = now
	}
	e.mu.Unlock()
	if !due {
		return nil
	}
	st, err := e.store.OutboxStats(ctx)
	if err != nil || st.RejectedBatches == 0 {
		return err
	}
	_, err = e.store.ExpediteAll(ctx, now)
	return err
}

// finishCheckIn moves to checked_in once nothing is unconfirmed and HQ
// has answered since the check-in began (both by this node's clock).
// The move is a compare-and-set, so an operator's Secure or Reset at the
// same moment wins.
func (e *Engine) finishCheckIn(ctx context.Context, cfg store.Settings) error {
	st, err := e.store.OutboxStats(ctx)
	if err != nil {
		return err
	}
	e.mu.Lock()
	heard := !e.checkInStart.IsZero() && !e.heardLocal.Before(e.checkInStart)
	e.mu.Unlock()
	if st.Unconfirmed > 0 || st.PendingBatches > 0 || st.RejectedBatches > 0 || !heard {
		return nil
	}
	ok, err := e.store.SetRaceState(ctx, []string{store.RaceCheckingIn}, store.RaceCheckedIn, nil)
	if err != nil || !ok {
		return err
	}
	e.log.Info("checkpoint: final check-in complete; everything confirmed by HQ")
	e.setLastState(store.RaceCheckedIn)
	return e.runCheckedInHook(ctx, true)
}

// runCheckedInHook runs OnCheckedIn on entering checked_in (including
// after a restart) and retries it each tick until it succeeds.
func (e *Engine) runCheckedInHook(ctx context.Context, entering bool) error {
	e.mu.Lock()
	if entering {
		e.hookPending = true
	}
	pending := e.hookPending
	e.mu.Unlock()
	if !pending || e.onCheckedIn == nil {
		return nil
	}
	if err := e.onCheckedIn(ctx); err != nil {
		return fmt.Errorf("checkpoint: after check-in: %w", err)
	}
	e.mu.Lock()
	e.hookPending = false
	e.mu.Unlock()
	return nil
}

// window returns the oldest max_in_flight of pending.
func window(pending []store.Batch, maxInFlight int) []store.Batch {
	return pending[:min(len(pending), max(maxInFlight, 0))]
}

// recoverUnbound runs Recover when a batch was attempted but never bound
// (a lost bind, or a send whose outcome was unknown), so its graywolf
// row is found before the batch would be sent again as a duplicate.
func (e *Engine) recoverUnbound(ctx context.Context, cfg store.Settings) error {
	unbound, err := e.store.UnboundAttempted(ctx)
	if err != nil || len(unbound) == 0 {
		return err
	}
	return e.Recover(ctx, cfg)
}

// flush batches the queue once the oldest entry has waited
// flush_after_sec or a full frame's worth is waiting.
func (e *Engine) flush(ctx context.Context, cfg store.Settings, now time.Time) error {
	n, oldest, err := e.store.QueueSummary(ctx, cfg.CheckpointCode)
	if err != nil || n == 0 {
		return err
	}
	maxLen := e.maxText(cfg)
	// Once the keypad is closed nothing else will join the queue.
	aged := cfg.RaceState != store.RaceActive ||
		now.Sub(oldest) >= time.Duration(cfg.FlushAfterSec)*time.Second
	if !aged && n < fullBatchEntries(maxLen) {
		return nil
	}
	for {
		b, err := e.store.CreateBatch(ctx, cfg.CheckpointCode, maxLen, now)
		if err != nil || b == nil {
			return err
		}
		e.log.Debug("checkpoint: batch created", "cp", b.CPCode, "seq", b.Seq, "text", b.Text)
	}
}

// transmit sends due batches within the window of the oldest
// max_in_flight unACKed ones, so a dead link costs at most
// max_in_flight frames per backoff period. The attempt is recorded
// before the send, so a DB error never leaves a frame on air without a
// saved retry schedule. One batch's failure doesn't stop the others.
func (e *Engine) transmit(ctx context.Context, cfg store.Settings, now time.Time) error {
	pending, err := e.store.ListPendingBatches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range window(pending, cfg.MaxInFlight) {
		if b.NextTxAt.After(now) {
			continue
		}
		attempt := b.Attempts + 1
		err := e.store.MarkTransmitted(ctx, b.ID, now, now.Add(retryBackoff(attempt)))
		if errors.Is(err, store.ErrNotFound) {
			continue // ACKed or parked since it was listed
		}
		if err != nil {
			return err
		}
		if err := e.send(ctx, cfg, b, now); err != nil {
			errs = append(errs, fmt.Errorf("checkpoint: send batch %s/%d: %w", b.CPCode, b.Seq, err))
			continue
		}
		e.log.Debug("checkpoint: batch sent", "cp", b.CPCode, "seq", b.Seq, "attempt", attempt)
	}
	return errors.Join(errs...)
}

// send transmits batch b once: a resend of its graywolf row if it has
// one, otherwise a new message bound to b. sentAt is the attempt time
// MarkTransmitted recorded.
//
// Failure handling depends on what is known. graywolf answered with an
// error status: nothing went on air, so the attempt is rolled back,
// except a 400 (refused), which keeps its retry-ladder slot so a
// systemic refusal (bad path) doesn't hammer graywolf every tick. A
// transport error (timeout, reset): the request may have reached
// graywolf, so the attempt is kept and recoverUnbound looks for the row
// before the batch is sent again.
func (e *Engine) send(ctx context.Context, cfg store.Settings, b store.Batch, sentAt time.Time) error {
	e.ensurePeer(ctx, cfg.HQCall)
	if b.GWMessageID != nil {
		_, err := e.gw.ResendMessage(ctx, *b.GWMessageID)
		switch {
		case err == nil:
			return nil
		case graywolf.IsConflict(err):
			// graywolf resends only a failed row; with its retries off a
			// row never fails, so this is the usual case (contract test,
			// 2026-10-10). If HQ already ACKed the row, take that;
			// otherwise send the batch again as a new message.
			if done, err := e.settledRow(ctx, cfg.HQCall, *b.GWMessageID); done || err != nil {
				return err
			}
			if err := e.releaseForNewCopy(ctx, b); err != nil {
				return ignoreSettled(err)
			}
		case !graywolf.IsNotFound(err):
			e.afterFailure(ctx, b, sentAt, err)
			return err
		default:
			e.log.Info("checkpoint: graywolf row gone; sending batch as new", "cp", b.CPCode, "seq", b.Seq)
			if err := e.releaseForNewCopy(ctx, b); err != nil {
				return ignoreSettled(err)
			}
		}
	}
	msg, err := e.gw.SendMessage(ctx, e.request(cfg, cfg.HQCall, b.Text, b.ClientID))
	if err != nil {
		e.afterFailure(ctx, b, sentAt, err)
		return err
	}
	e.setRefusal(Refusal{})
	wctx, cancel := detached(ctx)
	defer cancel()
	err = e.store.BindMessage(wctx, b.ID, msg.ID, msg.MsgID)
	if err != nil {
		err = e.store.BindMessage(wctx, b.ID, msg.ID, msg.MsgID) // one retry
	}
	if err != nil {
		// On air but unbound: recoverUnbound rebinds it next tick, and
		// HQ's (cp, seq, text) dedup absorbs any duplicate.
		return fmt.Errorf("bind to graywolf row %d: %w", msg.ID, err)
	}
	return nil
}

// errBatchSettled: the batch was acked or parked while a retransmit was
// being prepared, so there is nothing to send.
var errBatchSettled = errors.New("checkpoint: batch settled meanwhile")

func ignoreSettled(err error) error {
	if errors.Is(err, errBatchSettled) {
		return nil
	}
	return err
}

// settledRow reports whether graywolf row id, addressed to the current
// HQ, is already acked or rejected, applying that status to its batch,
// so a refused resend doesn't put a confirmed batch on air again. A row
// for a previous HQ call never settles the batch: it goes to the new HQ.
func (e *Engine) settledRow(ctx context.Context, hq string, id uint64) (bool, error) {
	m, err := e.gw.GetMessage(ctx, id)
	if err != nil {
		if graywolf.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !strings.EqualFold(m.ToCall, hq) || (m.Status != graywolf.StatusAcked && m.Status != graywolf.StatusRejected) {
		return false, nil
	}
	return true, e.applyStatus(ctx, hq, m)
}

// releaseForNewCopy unbinds b from its old row before it goes out as a
// new message, so a failed bind afterwards leaves it unbound and crash
// recovery binds the new row (store.ReleaseBinding). The old row stays
// a linked copy.
func (e *Engine) releaseForNewCopy(ctx context.Context, b store.Batch) error {
	if err := e.store.ReleaseBinding(ctx, b.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errBatchSettled
		}
		// Nothing went on air; the kept attempt retries at the next rung.
		return err
	}
	return nil
}

func (e *Engine) afterFailure(ctx context.Context, b store.Batch, sentAt time.Time, err error) {
	var apiErr *graywolf.APIError
	switch {
	case !errors.As(err, &apiErr):
		return // outcome unknown: keep the attempt (see send)
	case apiErr.StatusCode == 400:
		e.setRefusal(Refusal{At: e.now(), Reason: apiErr.Message})
		e.log.Warn("checkpoint: graywolf refused batch; check settings", "cp", b.CPCode, "seq", b.Seq, "reason", apiErr.Message)
		return
	}
	wctx, cancel := detached(ctx)
	defer cancel()
	if rerr := e.store.RestoreSchedule(wctx, b, sentAt); rerr != nil && !errors.Is(rerr, store.ErrNotFound) {
		e.log.Warn("checkpoint: restore batch schedule", "cp", b.CPCode, "seq", b.Seq, "err", rerr)
	}
}

func (e *Engine) request(cfg store.Settings, to, text, clientID string) graywolf.SendRequest {
	req := graywolf.SendRequest{To: to, Text: text, Path: cfg.Path, ClientID: clientID}
	if cfg.GWChannel > 0 {
		ch := cfg.GWChannel
		req.Channel = &ch
	}
	return req
}

// poll reads in-flight batches' status straight from graywolf every
// pollEvery, for status changes the inbox feed skipped.
func (e *Engine) poll(ctx context.Context, cfg store.Settings, now time.Time) error {
	e.mu.Lock()
	due := now.Sub(e.lastPoll) >= pollEvery
	if due {
		e.lastPoll = now
	}
	e.mu.Unlock()
	if !due {
		return nil
	}
	pending, err := e.store.ListPendingBatches(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, b := range window(pending, cfg.MaxInFlight) {
		ids, err := e.store.EarlierCopies(ctx, b.ID, pollEarlierCopies)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if b.GWMessageID != nil {
			ids = append([]uint64{*b.GWMessageID}, ids...)
		}
		for _, id := range ids {
			m, err := e.gw.GetMessage(ctx, id)
			if err != nil {
				if !graywolf.IsNotFound(err) { // gone: the next retransmit sends it as new
					errs = append(errs, err)
				}
				continue
			}
			errs = append(errs, e.applyStatus(ctx, cfg.HQCall, m))
			if m.Status == graywolf.StatusAcked {
				break // confirmed: no need to read older copies
			}
		}
	}
	return errors.Join(errs...)
}

// heartbeat sends "RC1 H" every heartbeat_sec (and on the first tick).
func (e *Engine) heartbeat(ctx context.Context, cfg store.Settings, now time.Time) error {
	e.mu.Lock()
	due := !now.Before(e.nextHeartbeat)
	e.mu.Unlock()
	if !due {
		return nil
	}
	last, err := e.store.LastSeq(ctx, cfg.CheckpointCode)
	if err != nil {
		return err
	}
	raceNow, _ := e.clock.Now()
	// Past Open, the checkpoint is closed (4.7): it says so, so HQ shows
	// it closed rather than quiet once it packs up and travels.
	closed := cfg.RaceState != store.RaceActive && cfg.RaceState != store.RaceSetup
	text, err := wire.EncodeHeartbeat(wire.Heartbeat{CP: cfg.CheckpointCode, LastSeq: last, Time: wire.TimeOfDayOf(raceNow), Closed: closed})
	if err != nil {
		return err
	}
	e.ensurePeer(ctx, cfg.HQCall)
	msg, sendErr := e.gw.SendMessage(ctx, e.request(cfg, cfg.HQCall, text, ""))
	next := now.Add(time.Duration(cfg.HeartbeatSec) * time.Second)
	if sendErr != nil {
		next = now.Add(heartbeatRetry)
	}
	e.mu.Lock()
	e.nextHeartbeat = next
	e.mu.Unlock()
	if sendErr != nil {
		return fmt.Errorf("checkpoint: send heartbeat: %w", sendErr)
	}
	wctx, cancel := detached(ctx)
	defer cancel()
	_, err = e.store.RecordGWRow(wctx, msg.ID, store.GWRowHeartbeat)
	return err
}

// HandleOutbound applies the delivery status of an RC1 DM this station
// sent (inbox.Dispatcher). Idempotent: rows reappear on every change.
func (e *Engine) HandleOutbound(ctx context.Context, m graywolf.Message) error {
	return e.applyStatus(ctx, e.currentHQ(), m)
}

func (e *Engine) applyStatus(ctx context.Context, hq string, m graywolf.Message) error {
	if hq == "" || !strings.EqualFold(m.ToCall, hq) {
		return nil
	}
	contactAt := e.now()
	if m.AckedAt != nil {
		contactAt = *m.AckedAt
	}
	// A re-delivered old ACK carries graywolf's original ACK time; one
	// without it counts as contact only if it confirms a batch now, so a
	// replayed row can't make a dead link look alive.
	noteContact := func(confirmed bool) {
		if confirmed || m.AckedAt != nil {
			e.markContact(contactAt)
		}
	}
	switch m.Status {
	case graywolf.StatusAcked:
		b, err := e.store.AckBatchByMessage(ctx, m.ID, contactAt)
		if err != nil {
			return err
		}
		noteContact(b != nil)
		if b != nil {
			e.log.Debug("checkpoint: batch confirmed", "cp", b.CPCode, "seq", b.Seq)
			e.expedite(ctx)
		}
	case graywolf.StatusRejected:
		b, err := e.store.RejectBatchByMessage(ctx, m.ID)
		if err != nil {
			return err
		}
		noteContact(b != nil)
		if b != nil {
			e.log.Warn("checkpoint: batch rejected by HQ; parked", "cp", b.CPCode, "seq", b.Seq)
		}
	}
	return nil
}

// expedite is fast retransmit: an ACK that just confirmed one of our
// batches proves the link works, so batches sent at least
// fastRetransmitAfter ago without an ACK were lost; make them due now.
// Only a confirming ACK triggers it, so each pending batch triggers it at
// most once and a spoofer can't drive an airtime storm.
func (e *Engine) expedite(ctx context.Context) {
	now := e.now()
	n, err := e.store.ExpediteUnacked(ctx, now.Add(-fastRetransmitAfter), now)
	if err != nil {
		e.log.Warn("checkpoint: expedite unacked batches", "err", err)
		return
	}
	if n > 0 {
		e.log.Debug("checkpoint: HQ reachable; resending overdue batches", "count", n)
	}
}

// HandleInbound processes an RC1 DM addressed to this station
// (inbox.Dispatcher). Only gap requests from the configured HQ call act;
// everything else is logged and ignored (returns nil: bad input is not
// worth retrying).
func (e *Engine) HandleInbound(ctx context.Context, m graywolf.Message) error {
	hq := e.currentHQ()
	if hq == "" || !strings.EqualFold(m.FromCall, hq) {
		e.log.Info("checkpoint: ignoring RC1 from non-HQ station", "from", m.FromCall)
		return nil
	}
	msg, err := wire.Decode(m.Text)
	if err != nil {
		e.log.Warn("checkpoint: undecodable RC1 from HQ", "text", m.Text, "err", err)
		return nil
	}
	g, ok := msg.(*wire.GapRequest)
	if !ok {
		return nil // probes are handled by the link check (phase 12)
	}
	n, err := e.store.RequeueSeqs(ctx, g.CP, g.Seqs, e.now())
	if err != nil {
		return err
	}
	e.log.Info("checkpoint: gap request", "cp", g.CP, "asked", len(g.Seqs), "requeued", n)
	e.markContact(e.now())
	return nil
}

// Recover binds batches whose send may have reached graywolf without
// being bound (a crash, a lost bind, or an unknown send outcome; spec
// 4.1.7). It matches each against graywolf's sent folder by client_id,
// then by exact text (unique per cp and seq), considering only rows
// created after the batch and not already recorded by the app, so a
// row from an earlier race with identical text (after a reset) can't
// be taken. Batches with no match stay unbound and are sent as new.
func (e *Engine) Recover(ctx context.Context, cfg store.Settings) error {
	unbound, err := e.store.UnboundAttempted(ctx)
	if err != nil || len(unbound) == 0 {
		return err
	}
	var rows []graywolf.Message
	_, err = e.gw.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderSent, Peer: cfg.HQCall, Since: unbound[0].CreatedAt.Add(-recoverSkew)},
		func(ch graywolf.MessageChange) error {
			if m := ch.Message; m != nil && m.Direction == "out" {
				rows = append(rows, *m)
			}
			return nil
		})
	if err != nil {
		return fmt.Errorf("checkpoint: recover: list sent: %w", err)
	}
	for _, b := range unbound {
		m, ok, err := e.matchRow(ctx, b, rows)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		if err := e.store.BindMessage(ctx, b.ID, m.ID, m.MsgID); err != nil {
			return fmt.Errorf("checkpoint: recover batch %s/%d: %w", b.CPCode, b.Seq, err)
		}
		e.log.Info("checkpoint: recovered batch send", "cp", b.CPCode, "seq", b.Seq, "row", m.ID)
	}
	return nil
}

// matchRow finds b's row: newest unrecorded row created after b with
// b's client_id, else with b's exact text.
func (e *Engine) matchRow(ctx context.Context, b store.Batch, rows []graywolf.Message) (graywolf.Message, bool, error) {
	var byClient, byText *graywolf.Message
	for i := range rows {
		m := &rows[i]
		if m.CreatedAt != nil && m.CreatedAt.Before(b.CreatedAt.Add(-recoverSkew)) {
			continue
		}
		known, err := e.store.KnownGWRow(ctx, m.ID)
		if err != nil {
			return graywolf.Message{}, false, err
		}
		if known {
			continue
		}
		if b.ClientID != "" && m.ClientID == b.ClientID {
			byClient = m
		}
		if m.Text == b.Text {
			byText = m
		}
	}
	switch {
	case byClient != nil:
		return *byClient, true, nil
	case byText != nil:
		return *byText, true, nil
	default:
		return graywolf.Message{}, false, nil
	}
}
