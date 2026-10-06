package linkcheck

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// Timing (spec 4.8.1-2).
const (
	// replyQuiet: the responder replies this long after the last probe
	// it heard, unless it heard the final probe (then at once).
	replyQuiet = 30 * time.Second
	// replyResend: an unACKed reply is resent this often, maxReplyResends times.
	replyResend     = 30 * time.Second
	maxReplyResends = 2
	// ackWait: how long the prober waits for the last probe's ACK once
	// the reply is in.
	ackWait = 30 * time.Second
	// replyWait: how long after the last probe the prober waits for a
	// reply (covers the quiet time and both resends).
	replyWait = replyQuiet + maxReplyResends*replyResend + 30*time.Second
	// replyBudget caps reply transmissions (resends included) a
	// responder makes per hour, so forged probes can't fill the channel.
	// (graywolf ACKs every DM itself; that is outside the app's control.)
	replyBudget = 40
	// packetLookback bounds the packet-log reads.
	packetLookback = 2 * time.Minute
	packetLimit    = 500
	// TickEvery is how often Run ticks.
	TickEvery = time.Second
	// idlePoll is how often an idle service looks for work: a run
	// requested from the web or CLI starts within this, and a scheduled
	// reply may go out up to this late.
	idlePoll = 5 * time.Second
)

// Graywolf is the slice of graywolf's API the link check uses.
type Graywolf interface {
	SendMessage(ctx context.Context, req graywolf.SendRequest) (graywolf.Message, error)
	ResendMessage(ctx context.Context, id uint64) (graywolf.Message, error)
	GetMessage(ctx context.Context, id uint64) (graywolf.Message, error)
	ListPackets(ctx context.Context, q graywolf.PacketQuery) ([]graywolf.Packet, error)
}

// Peers turns graywolf's own retries off for a peer before a send, so
// each probe is exactly one frame.
type Peers interface {
	Ensure(ctx context.Context, call string) error
}

// Config wires a Service.
type Config struct {
	Store    *store.Store
	Graywolf Graywolf
	Peers    Peers // optional
	Now      func() time.Time
	Logger   *slog.Logger
}

// Service runs this node's link checks as prober and answers peers'
// probes as responder.
type Service struct {
	store *store.Store
	gw    Graywolf
	peers Peers
	now   func() time.Time
	log   *slog.Logger

	mu        sync.Mutex
	idleUntil time.Time // no store reads before this unless woken
}

// New returns a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Graywolf == nil {
		return nil, errors.New("linkcheck: store and graywolf are required")
	}
	s := &Service{store: cfg.Store, gw: cfg.Graywolf, peers: cfg.Peers, now: cfg.Now, log: cfg.Logger}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	return s, nil
}

// Run ticks until ctx ends.
func (s *Service) Run(ctx context.Context) error {
	t := time.NewTicker(TickEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			if err := s.Tick(ctx); err != nil {
				s.log.Warn("linkcheck: tick", "err", err)
			}
		}
	}
}

// Tick advances the running check (or starts a requested one) and sends
// due replies.
//
// An idle tick costs two indexed queries; settings are read only when
// there is work.
func (s *Service) Tick(ctx context.Context) error {
	now := s.now()
	s.mu.Lock()
	idle := now.Before(s.idleUntil)
	s.mu.Unlock()
	if idle {
		return nil
	}
	pending, err := s.store.PendingLinkCheck(ctx)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	due, derr := s.store.DueLinkResponses(ctx, now)
	if derr != nil {
		return derr
	}
	if err != nil && len(due) == 0 {
		s.mu.Lock()
		s.idleUntil = now.Add(idlePoll)
		s.mu.Unlock()
		return nil
	}
	cfg, cerr := s.store.GetSettings(ctx)
	if cerr != nil {
		return cerr
	}
	var errs []error
	if err == nil {
		errs = append(errs, s.tickProber(ctx, cfg, pending, now))
	}
	for _, r := range due {
		errs = append(errs, s.reply(ctx, cfg, r, now))
	}
	return errors.Join(errs...)
}

func (s *Service) request(cfg store.Settings, to, text string) graywolf.SendRequest {
	req := graywolf.SendRequest{To: to, Text: text, Path: cfg.Path}
	if cfg.GWChannel > 0 {
		ch := cfg.GWChannel
		req.Channel = &ch
	}
	return req
}

func (s *Service) ensure(ctx context.Context, call string) {
	if s.peers == nil {
		return
	}
	if err := s.peers.Ensure(ctx, call); err != nil {
		s.log.Warn("linkcheck: turn off graywolf retries for peer", "peer", call, "err", err)
	}
}

func (s *Service) recordRow(ctx context.Context, id uint64, kind string) {
	if _, err := s.store.RecordGWRow(ctx, id, kind); err != nil {
		s.log.Warn("linkcheck: record graywolf row", "row", id, "kind", kind, "err", err)
	}
}

// HandleInbound processes a decoded RC1 P or Q addressed to this node.
// Bad or unwanted input returns nil; only store failures are errors.
func (s *Service) HandleInbound(ctx context.Context, m graywolf.Message, msg wire.Message) error {
	sender := strings.ToUpper(strings.TrimSpace(m.FromCall))
	if !store.ValidStationCall(sender) {
		return nil
	}
	// A probe schedules a reply and a reply may finish a run: look for
	// work on the next tick (after this write, so the tick sees it).
	defer s.wake()
	switch msg := msg.(type) {
	case *wire.Probe:
		s.recordRow(ctx, m.ID, store.GWRowInbound)
		return s.onProbe(ctx, m, sender, msg)
	case *wire.ProbeReply:
		s.recordRow(ctx, m.ID, store.GWRowInbound)
		return s.onReply(ctx, sender, msg)
	}
	return nil
}

// wake makes the next Tick look for work.
func (s *Service) wake() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idleUntil = time.Time{}
}

// HandleOutbound sees status changes of this node's probe and reply rows.
func (s *Service) HandleOutbound(ctx context.Context, m graywolf.Message) error {
	if m.Status != graywolf.StatusAcked {
		return nil
	}
	ackedAt := s.now()
	if m.AckedAt != nil {
		ackedAt = *m.AckedAt
	}
	if err := s.markProbeAcked(ctx, m, ackedAt); err != nil {
		return err
	}
	_, err := s.store.AckLinkReply(ctx, m.ID, ackedAt)
	return err
}

// markProbeAcked records the ACK of probe row m (if it is one), once.
func (s *Service) markProbeAcked(ctx context.Context, m graywolf.Message, fallback time.Time) error {
	p, err := s.store.LinkProbeByMessage(ctx, m.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil || p.AckedAt != nil {
		return err
	}
	ackedAt := fallback
	if m.AckedAt != nil {
		ackedAt = *m.AckedAt
	}
	sent := p.SentAt
	if m.SentAt != nil {
		sent = m.SentAt // graywolf's own times, full precision
	}
	rtt := 0
	if sent != nil {
		rtt = int(max(ackedAt.Sub(*sent), 0) / time.Millisecond)
	}
	_, err = s.store.AckLinkProbe(ctx, m.ID, ackedAt, rtt)
	return err
}

func itoa(n int) string { return strconv.Itoa(n) }

// levelAt finds the receive level and path of a frame heard from
// sender: the newest packet-log entry from sender matching match.
func (s *Service) levelAt(ctx context.Context, sender string, since time.Time, match func(*graywolf.PacketMessage) bool) (levels []int, via string) {
	pk, err := s.gw.ListPackets(ctx, graywolf.PacketQuery{Since: since, Type: "message", Direction: "RX", Limit: packetLimit})
	if err != nil {
		s.log.Debug("linkcheck: read packet log", "err", err)
		return nil, ""
	}
	for _, p := range pk {
		if p.Decoded == nil || p.Decoded.Message == nil || !strings.EqualFold(p.Decoded.Source, sender) || !match(p.Decoded.Message) {
			continue
		}
		if p.AudioLevel != nil {
			levels = append(levels, clampLevel(p.AudioLevel.LevelDBFS))
		}
		via = p.Via
	}
	return levels, via
}

func clampLevel(dbfs float64) int {
	return int(math.Max(float64(wire.MinLevel), math.Min(0, math.Round(dbfs))))
}

func medianInt(v []int) *int {
	if len(v) == 0 {
		return nil
	}
	s := slices.Clone(v)
	slices.Sort(s)
	m := s[len(s)/2]
	if len(s)%2 == 0 {
		m = (s[len(s)/2-1] + s[len(s)/2]) / 2
	}
	return &m
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}
