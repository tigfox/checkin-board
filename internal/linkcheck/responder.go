package linkcheck

import (
	"context"
	"errors"
	"strings"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// answering reports whether this node answers probes now: only before
// and during the race (a secured checkpoint stays quiet).
func answering(cfg store.Settings) bool {
	return cfg.RaceState == store.RaceSetup || cfg.RaceState == store.RaceActive
}

// onProbe records a probe heard and schedules the reply (spec 4.8.2).
func (s *Service) onProbe(ctx context.Context, m graywolf.Message, sender string, p *wire.Probe) error {
	cfg, err := s.store.GetSettings(ctx)
	if err != nil {
		return err
	}
	if !answering(cfg) {
		s.log.Info("linkcheck: ignoring probe outside setup/active", "from", sender, "state", cfg.RaceState)
		return nil
	}
	unknown := false
	switch cfg.Role {
	case store.RoleCheckpoint:
		if !strings.EqualFold(sender, cfg.HQCall) {
			s.log.Info("linkcheck: ignoring probe from a station other than HQ", "from", sender)
			return nil
		}
	case store.RoleHQ:
		cps, err := s.store.ListCheckpoints(ctx)
		if err != nil {
			return err
		}
		known := len(cps) == 0
		for _, c := range cps {
			if strings.EqualFold(c.ExpectedCall, sender) {
				known = true
			}
		}
		unknown = !known
	default:
		return nil
	}
	now := s.now()
	heardAt := now
	if m.ReceivedAt != nil {
		heardAt = *m.ReceivedAt
	}
	levels, via := s.levelAt(ctx, sender, heardAt.Add(-packetLookback), func(pm *graywolf.PacketMessage) bool { return pm.Text == m.Text })
	if m.Via != "" {
		via = m.Via
	}
	r, err := s.store.RecordProbeHeard(ctx, store.ProbeHeard{
		PeerCall: sender, ProberCode: p.CP, Run: int(p.Run), Total: int(p.Total), Idx: int(p.Index),
		At: heardAt, Level: medianInt(levels), Via: viaField(via), UnknownPeer: unknown,
	})
	if err != nil {
		if errors.Is(err, store.ErrInvalidInput) {
			return nil
		}
		return err
	}
	if r.ReplySentAt != nil || r.ReplyAttempts > 0 {
		return nil // already answered; the prober counts from that reply
	}
	due := r.LastHeardAt.Add(replyQuiet)
	if int(p.Index) == r.Total {
		due = now
	}
	_, err = s.store.ScheduleLinkReply(ctx, r.ID, due)
	return err
}

// viaField keeps only a path the RC1 Q grammar can carry.
func viaField(via string) string {
	via = strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(via), "*"))
	if _, err := wire.EncodeProbeReply(wire.ProbeReply{CP: "X", Run: 1, Heard: []uint8{1}, Level: wire.LevelUnknown, Via: via}); err != nil {
		return ""
	}
	return via
}

// reply sends, resends or gives up on one response's reply. Every
// transmission counts against the hourly budget, resends included.
func (s *Service) reply(ctx context.Context, cfg store.Settings, r store.LinkResponse, now time.Time) error {
	switch {
	case !answering(cfg), r.ReplyAckedAt != nil, r.ReplyAttempts > maxReplyResends,
		cfg.Role == store.RoleCheckpoint && !strings.EqualFold(r.PeerCall, cfg.HQCall),
		cfg.Role == "":
		return s.store.StopLinkReply(ctx, r.ID)
	}
	n, err := s.store.CountLinkSendsSince(ctx, now.Add(-time.Hour))
	if err != nil {
		return err
	}
	if n >= replyBudget {
		s.log.Warn("linkcheck: reply budget used up; not answering", "peer", r.PeerCall, "budget_per_hour", replyBudget)
		return s.store.StopLinkReply(ctx, r.ID)
	}
	next := now.Add(replyResend)
	if r.ReplyGWID != nil {
		_, err := s.gw.ResendMessage(ctx, *r.ReplyGWID)
		switch {
		case err == nil:
			_, err = s.store.MarkLinkReplySent(ctx, r.ID, *r.ReplyGWID, now, next)
			return err
		case graywolf.IsConflict(err): // still going out: count it as a try
			_, err = s.store.RescheduleLinkReply(ctx, r.ID, now.Add(5*time.Second), true)
			return err
		case !graywolf.IsNotFound(err):
			s.log.Warn("linkcheck: resend reply", "peer", r.PeerCall, "err", err)
			_, err = s.store.RescheduleLinkReply(ctx, r.ID, next, true)
			return err
		} // the row is gone: send it as new
	}
	lvl := wire.LevelUnknown
	if r.Level != nil {
		lvl = wire.Level(*r.Level)
	}
	text, err := wire.EncodeProbeReply(wire.ProbeReply{CP: r.ProberCode, Run: uint16(r.Run), Heard: r.HeardIndices(), Level: lvl, Via: r.Via})
	if err != nil {
		return errors.Join(err, s.store.StopLinkReply(ctx, r.ID))
	}
	s.ensure(ctx, r.PeerCall)
	msg, err := s.gw.SendMessage(ctx, s.request(cfg, r.PeerCall, text))
	if err != nil {
		s.log.Warn("linkcheck: send reply", "peer", r.PeerCall, "err", err)
		_, err = s.store.RescheduleLinkReply(ctx, r.ID, next, true)
		return err
	}
	s.recordRow(ctx, msg.ID, store.GWRowReply)
	_, err = s.store.MarkLinkReplySent(ctx, r.ID, msg.ID, now, next)
	return err
}
