// Package radiobackend lets a node use a UV-K5 V3 / UV-K1 radio (custom
// APRS firmware, USB-C) instead of graywolf (phase 17b). It implements
// the graywolf API the engines use (app.Graywolf) on top of the radio's
// serial protocol, so the checkpoint, HQ, link check and inbox code
// don't change:
//
//   - message rows like graywolf's, kept in the node's database (the
//     radio has no history), with a change cursor and event hints;
//   - a sender that queues rows into the radio's 4-deep queue only while
//     it has free slots (it refuses when full, never replacing);
//   - acks matched to rows by peer and the radio's line number; SENT and
//     DROP lines move rows to sent_rf and failed;
//   - inbound messages to this station stored once (the radio acks them
//     itself), duplicates dropped as graywolf does.
//
// Retries stay in the engines, as with graywolf's own retries off.
package radiobackend

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

const (
	// statusSubmitted: in the radio's queue, not on air yet.
	statusSubmitted = graywolf.StatusTxSubmitted
	// submittedTimeout: a submitted row with no SENT or DROP this long
	// after it was queued (the radio gives up at 60 s and says so; this
	// covers a DROP lost to a reconnect) counts as failed.
	submittedTimeout = 90 * time.Second
	// ackWindow: acks match rows sent this recently (line numbers wrap
	// at 99).
	ackWindow = 30 * time.Minute
	// dedupWindow is graywolf's: a repeat within it isn't stored again.
	dedupWindow = 5 * time.Minute
	maxPackets  = 2000
	senderTick  = time.Second
	// reviveOverdue: a message the radio took this long ago with no
	// SENT/DROP, and no line at all for as long, means the monitor lines
	// may have stopped (a timed-out write to the host): turn them back on.
	reviveOverdue = 30 * time.Second
	// reviveEvery refreshes "monitor on" anyway: lost lines for incoming
	// messages show nowhere else.
	reviveEvery = 2 * time.Minute
	minBackoff  = 2 * time.Second
	maxBackoff  = 30 * time.Second
	opTimeout   = 5 * time.Second
)

// Session is what the backend uses of a radio session (uvk5.Session).
type Session interface {
	Send(ctx context.Context, to, text string) (uvk5.SendResult, error)
	Events() <-chan any
	Done() <-chan struct{}
	Err() error
	Settings() uvk5.Settings
	Capabilities() uvk5.Capabilities
	FirmwareVersion() string
	Status(ctx context.Context) (uvk5.Status, error)
	SetCallsign(ctx context.Context, cs string) error
	Close() error
}

// Config configures a Backend.
type Config struct {
	Store *store.Store
	// Dial connects to the radio (uvk5.Dial in production).
	Dial   func(ctx context.Context) (Session, error)
	Now    func() time.Time
	Logger *slog.Logger
}

// Backend is the radio as the engines see graywolf. Safe for concurrent use.
type Backend struct {
	st   *store.Store
	dial func(ctx context.Context) (Session, error)
	now  func() time.Time
	log  *slog.Logger
	wake chan struct{}

	mu         sync.Mutex
	sess       Session
	free       int
	connErr    error
	connected  time.Time
	callsign   string // the last callsign read from the radio
	subs       map[chan graywolf.Event]struct{}
	heard      map[string]time.Time
	packets    []graywolf.Packet
	rssi       map[string]int
	lastLine   time.Time     // the last monitor line from the radio
	lastRevive time.Time     // the last "monitor on" (connect, or a revival)
	lent       string        // who has the radio's port ("": the backend)
	returned   chan struct{} // closed when it's given back
	dialing    bool
}

// New returns a backend; Run connects it.
func New(cfg Config) (*Backend, error) {
	if cfg.Store == nil || cfg.Dial == nil {
		return nil, errors.New("radiobackend: Store and Dial are required")
	}
	b := &Backend{st: cfg.Store, dial: cfg.Dial, now: cfg.Now, log: cfg.Logger, wake: make(chan struct{}, 1),
		subs: map[chan graywolf.Event]struct{}{}, heard: map[string]time.Time{}, rssi: map[string]int{}}
	if b.now == nil {
		b.now = time.Now
	}
	if b.log == nil {
		b.log = slog.New(slog.DiscardHandler)
	}
	b.connErr = errors.New("radio not connected yet")
	return b, nil
}

// Run keeps a session to the radio and moves rows until ctx ends.
func (b *Backend) Run(ctx context.Context) {
	go b.sender(ctx)
	backoff := minBackoff
	for ctx.Err() == nil {
		// "Not lent, so dial" and "not lent, so use the session" are each
		// one step under b.mu, so Lend can't slip between them.
		if lent := b.beginDial(); lent != nil {
			select {
			case <-lent:
			case <-ctx.Done():
				return
			}
			continue
		}
		sess, err := b.dial(ctx)
		if !b.endDial(sess, err) {
			continue // lent meanwhile: the session (if any) is closed
		}
		if err != nil {
			b.setDown(err)
			b.log.Warn("radio: can't connect", "err", err, "retry_in", backoff)
			if sleep(ctx, backoff) != nil {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = minBackoff
		b.logUp(sess)
		b.events(ctx, sess)
		_ = sess.Close()
		err = sess.Err()
		if err == nil {
			err = ctx.Err()
		}
		if lent := b.Lent(); lent != "" {
			continue // Lend set the reason
		}
		b.setDown(fmt.Errorf("radio disconnected: %w", err))
		if ctx.Err() == nil {
			b.log.Warn("radio: session ended", "err", err)
		}
	}
}

func (b *Backend) up(sess Session) {
	b.mu.Lock()
	b.install(sess)
	b.mu.Unlock()
	b.logUp(sess)
}

// install makes sess the backend's session. Caller holds b.mu.
func (b *Backend) install(sess Session) {
	b.sess, b.free, b.connErr, b.connected = sess, int(sess.Capabilities().QueueDepth), nil, b.now()
	b.lastLine, b.lastRevive = b.connected, b.connected
	b.callsign = sess.Settings().Callsign
}

func (b *Backend) logUp(sess Session) {
	caps := sess.Capabilities()
	b.log.Info("radio: connected", "firmware", sess.FirmwareVersion(), "callsign", sess.Settings().Callsign,
		"features", fmt.Sprintf("%#x", caps.Features), "queue", caps.QueueDepth)
	b.kick()
}

// beginDial marks a dial in progress, or returns the channel to wait on
// while the radio is lent.
func (b *Backend) beginDial() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lent != "" {
		return b.returned
	}
	b.dialing = true
	return nil
}

// endDial ends a dial: a session is installed unless the radio was lent
// meanwhile (then it's closed). It reports whether Run should go on with
// this dial's result.
func (b *Backend) endDial(sess Session, err error) bool {
	b.mu.Lock()
	b.dialing = false
	if b.lent != "" {
		b.mu.Unlock()
		if err == nil {
			_ = sess.Close()
		}
		return false
	}
	if err == nil {
		b.install(sess)
	}
	b.mu.Unlock()
	return true
}

func (b *Backend) setDown(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sess, b.free, b.connErr = nil, 0, err
}

func (b *Backend) session() (Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess == nil {
		return nil, b.connErr
	}
	return b.sess, nil
}

func (b *Backend) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// --- sending ---

func (b *Backend) sender(ctx context.Context) {
	t := time.NewTicker(senderTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-t.C:
		}
		b.expireSubmitted(ctx)
		b.reviveIfSilent(ctx)
		b.submit(ctx)
	}
}

// submit hands queued rows to the radio while it has free slots.
func (b *Backend) submit(ctx context.Context) {
	rows, err := b.st.RadioOutbound(ctx, graywolf.StatusQueued)
	if err != nil {
		b.log.Error("radio: list queued", "err", err)
		return
	}
	for _, row := range rows {
		b.mu.Lock()
		sess, free := b.sess, b.free
		b.mu.Unlock()
		if sess == nil || free <= 0 {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, opTimeout)
		res, err := sess.Send(sctx, row.ToCall, row.Text)
		cancel()
		var refused *uvk5.RefusedError
		switch {
		case errors.As(err, &refused) && refused.Reason == uvk5.ReasonFull:
			b.setFree(0)
			return
		case errors.As(err, &refused):
			b.update(ctx, row.ID, func(m *store.RadioMessage) bool {
				m.Status, m.FailureReason = graywolf.StatusFailed, "radio refused: "+refused.Reason.String()
				return true
			})
			continue
		case err != nil:
			b.log.Warn("radio: send", "id", row.ID, "err", err)
			return // the session is in trouble: try again later
		}
		b.setFree(int(res.FreeSlots))
		b.update(ctx, row.ID, func(m *store.RadioMessage) bool {
			m.Status, m.MsgID, m.CreatedAt = statusSubmitted, res.Line, b.now()
			return true
		})
	}
}

func (b *Backend) setFree(n int) {
	b.mu.Lock()
	b.free = n
	b.mu.Unlock()
}

func (b *Backend) freeOne() {
	b.mu.Lock()
	if b.sess != nil && b.free < int(b.sess.Capabilities().QueueDepth) {
		b.free++
	}
	b.mu.Unlock()
	b.kick()
}

// expireSubmitted fails rows the radio never reported on.
func (b *Backend) expireSubmitted(ctx context.Context) {
	rows, err := b.st.RadioOutbound(ctx, statusSubmitted)
	if err != nil {
		return
	}
	for _, row := range rows {
		if b.now().Sub(row.CreatedAt) >= submittedTimeout {
			b.update(ctx, row.ID, func(m *store.RadioMessage) bool {
				m.Status, m.FailureReason = graywolf.StatusFailed, "no word from the radio (disconnected?)"
				return true
			})
		}
	}
}

// --- receiving ---

func (b *Backend) events(ctx context.Context, sess Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Done():
			return
		case ev := <-sess.Events():
			b.handle(ctx, ev)
		}
	}
}

func (b *Backend) handle(ctx context.Context, ev any) {
	b.mu.Lock()
	b.lastLine = b.now()
	b.mu.Unlock()
	switch e := ev.(type) {
	case uvk5.Sent:
		b.settle(ctx, e.To, e.Line, func(m *store.RadioMessage) {
			now := b.now()
			m.Status, m.SentAt = graywolf.StatusSentRF, &now
		}, statusSubmitted)
		b.freeOne()
	case uvk5.Dropped:
		if e.Ack {
			b.log.Warn("radio: an ack was given up", "to", e.To, "id", e.Line, "reason", e.Reason)
			return
		}
		b.settle(ctx, e.To, e.Line, func(m *store.RadioMessage) {
			m.Status, m.FailureReason = graywolf.StatusFailed, "radio dropped it: "+e.Reason.String()
		}, statusSubmitted)
		b.freeOne()
	case uvk5.Heard:
		b.heardPacket(ctx, e.Packet)
	case uvk5.HeardRSSI:
		b.mu.Lock()
		if n := len(b.packets); n > 0 {
			b.rssi[strings.ToUpper(b.packets[n-1].Decoded.Source)] = e.DBm
		}
		b.mu.Unlock()
	}
}

// settle applies change to the row in statuses matching to/line.
func (b *Backend) settle(ctx context.Context, to, line string, change func(*store.RadioMessage), statuses ...string) {
	row, err := b.st.RadioMatchAck(ctx, to, line, b.now().Add(-ackWindow), statuses...)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			b.log.Error("radio: find row", "to", to, "line", line, "err", err)
		}
		return
	}
	b.update(ctx, row.ID, func(m *store.RadioMessage) bool { change(m); return true })
}

func (b *Backend) heardPacket(ctx context.Context, p uvk5.Packet) {
	msg, ok := uvk5.ParseMessage(p.Info)
	if !ok {
		return
	}
	b.logPacket(p, msg)
	own := b.ownCall()
	if own == "" || !strings.EqualFold(msg.Addressee, own) {
		return
	}
	switch {
	case msg.Ack || msg.Rej:
		b.settle(ctx, p.Src, msg.ID, func(m *store.RadioMessage) {
			now := b.now()
			if msg.Ack {
				m.Status, m.AckedAt = graywolf.StatusAcked, &now
			} else {
				m.Status = graywolf.StatusRejected
			}
			if m.SentAt == nil {
				m.SentAt = &now // the ack can beat the SENT line
			}
		}, statusSubmitted, graywolf.StatusSentRF)
	default:
		key := strings.ToUpper(p.Src) + "\x00" + msg.ID + "\x00" + msg.Text
		now := b.now()
		b.mu.Lock()
		last, seen := b.heard[key]
		b.heard[key] = now
		for k, t := range b.heard {
			if now.Sub(t) > dedupWindow {
				delete(b.heard, k)
			}
		}
		b.mu.Unlock()
		if seen && now.Sub(last) <= dedupWindow {
			return
		}
		row := &store.RadioMessage{Direction: "in", Status: graywolf.StatusReceived, FromCall: strings.ToUpper(p.Src),
			ToCall: strings.ToUpper(own), PeerCall: strings.ToUpper(p.Src), Text: msg.Text, MsgID: msg.ID,
			Via: strings.Join(p.Path, ","), Unread: true, ReceivedAt: &now, CreatedAt: now}
		if err := b.st.RadioInsert(ctx, row); err != nil {
			b.log.Error("radio: store a received message", "from", p.Src, "err", err)
			return
		}
		b.notify(row.ID, graywolf.EventReceived)
	}
}

func (b *Backend) logPacket(p uvk5.Packet, m uvk5.Message) {
	pk := graywolf.Packet{Timestamp: b.now(), Direction: "RX", Type: "message", Via: strings.Join(p.Path, ","),
		Decoded: &graywolf.DecodedPacket{Source: p.Src, Message: &graywolf.PacketMessage{
			Addressee: m.Addressee, Text: m.Text, MessageID: m.ID, IsAck: m.Ack}}}
	b.mu.Lock()
	b.packets = append(b.packets, pk)
	if len(b.packets) > maxPackets {
		b.packets = b.packets[len(b.packets)-maxPackets:]
	}
	b.mu.Unlock()
}

func (b *Backend) ownCall() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sess != nil {
		b.callsign = b.sess.Settings().Callsign
	}
	return b.callsign
}

// --- rows and events ---

func (b *Backend) update(ctx context.Context, id uint64, change func(*store.RadioMessage) bool) {
	row, err := b.st.RadioUpdate(ctx, id, change)
	if err != nil {
		b.log.Error("radio: update row", "id", id, "err", err)
		return
	}
	b.notify(row.ID, eventFor(row.Status))
}

func eventFor(status string) string {
	switch status {
	case graywolf.StatusAcked:
		return graywolf.EventAcked
	case graywolf.StatusRejected:
		return graywolf.EventRejected
	case graywolf.StatusSentRF:
		return graywolf.EventSentRF
	case graywolf.StatusFailed:
		return graywolf.EventFailed
	case graywolf.StatusReceived:
		return graywolf.EventReceived
	}
	return graywolf.EventUpdated
}

// notify sends a change hint to every stream; a slow subscriber catches
// up through CatchUp, as with graywolf.
func (b *Backend) notify(id uint64, event string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- graywolf.Event{Type: event, Change: graywolf.MessageChange{ID: id}}:
		default:
		}
	}
}

func apiErr(code int, path, msg string) error {
	return &graywolf.APIError{Method: "RADIO", Path: path, StatusCode: code, Message: msg}
}

func (b *Backend) toMessage(m store.RadioMessage) graywolf.Message {
	attempts := 0
	if m.SentAt != nil {
		attempts = 1
	}
	return graywolf.Message{ID: m.ID, Direction: m.Direction, Status: m.Status, FromCall: m.FromCall, ToCall: m.ToCall,
		PeerCall: m.PeerCall, OurCall: b.ownCall(), Text: m.Text, MsgID: m.MsgID, ThreadKind: graywolf.ThreadKindDM,
		ThreadKey: m.PeerCall, Source: "rf", Via: m.Via, Attempts: attempts, Unread: m.Unread,
		FailureReason: m.FailureReason, ClientID: m.ClientID, CreatedAt: ptr(m.CreatedAt), SentAt: m.SentAt,
		ReceivedAt: m.ReceivedAt, AckedAt: m.AckedAt}
}

func ptr[T any](v T) *T { return &v }

// Attach connects the backend to an open session without Run, for
// simulations that drive it with Step in simulated time.
func (b *Backend) Attach(sess Session) { b.up(sess) }

// Detach drops the session (the radio is unplugged), for simulations.
func (b *Backend) Detach(err error) {
	b.mu.Lock()
	sess := b.sess
	b.mu.Unlock()
	if sess != nil {
		_ = sess.Close()
	}
	b.setDown(err)
}

// Step handles the radio's pending events and moves rows without
// waiting: one Run iteration, for simulations.
func (b *Backend) Step(ctx context.Context) {
	if sess, err := b.session(); err == nil {
	drain:
		for {
			select {
			case ev := <-sess.Events():
				b.handle(ctx, ev)
			default:
				break drain
			}
		}
	}
	b.expireSubmitted(ctx)
	b.submit(ctx)
}

// reviver is a session whose monitor lines can be turned back on
// (uvk5.Session).
type reviver interface {
	ReviveMonitor(ctx context.Context, toggleDTR bool) error
}

// reviveIfSilent turns the radio's monitor lines back on when a report
// is overdue and nothing has been heard for as long (they may have
// stopped after a timed-out write to the host), and every reviveEvery
// anyway.
func (b *Backend) reviveIfSilent(ctx context.Context) {
	b.mu.Lock()
	sess, last, lastRevive := b.sess, b.lastLine, b.lastRevive
	b.mu.Unlock()
	r, ok := sess.(reviver)
	if !ok {
		return
	}
	now := b.now()
	overdue := false
	if now.Sub(last) >= reviveOverdue && now.Sub(lastRevive) >= reviveOverdue {
		if rows, err := b.st.RadioOutbound(ctx, statusSubmitted); err == nil {
			for _, row := range rows {
				if now.Sub(row.CreatedAt) >= reviveOverdue {
					overdue = true
					break
				}
			}
		}
	}
	if !overdue && now.Sub(lastRevive) < reviveEvery {
		return
	}
	b.mu.Lock()
	b.lastRevive = now
	b.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, opTimeout)
	defer cancel()
	err := r.ReviveMonitor(rctx, overdue)
	switch {
	case err != nil:
		b.log.Warn("radio: turn monitor lines back on", "err", err)
	case overdue:
		b.log.Warn("radio: no report lines with messages waiting; turned monitor lines back on", "silent_for", now.Sub(last).Round(time.Second))
	}
}
