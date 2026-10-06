package linkcheck

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// requestMaxAge: a request the service didn't start this soon (it was
// down) expires instead of transmitting later, unattended.
const requestMaxAge = time.Minute

func (s *Service) tickProber(ctx context.Context, cfg store.Settings, c store.LinkCheck, now time.Time) error {
	if c.State == store.LinkCheckRequested {
		if now.Sub(c.RequestedAt) > requestMaxAge {
			_, err := s.store.CancelLinkCheck(ctx, c.ID, "expired: the service didn't start it in time")
			return err
		}
		if reason := s.blocked(cfg, c); reason != "" {
			_, err := s.store.CancelLinkCheck(ctx, c.ID, reason)
			return err
		}
		run := 1 + rand.IntN(wire.MaxProbeRun)
		ok, err := s.store.StartLinkCheck(ctx, c.ID, run, now)
		if err != nil || !ok {
			return err // !ok: cancelled meanwhile
		}
		s.log.Info("linkcheck: started", "peer", c.PeerCall, "run", run, "probes", c.Count)
		c.State, c.Run, c.StartedAt = store.LinkCheckRunning, run, &now
	}
	return s.progress(ctx, cfg, c, now)
}

// blocked says why a run may not transmit now ("" if it may).
func (s *Service) blocked(cfg store.Settings, c store.LinkCheck) string {
	switch {
	case !answering(cfg):
		return "the race is " + cfg.RaceState
	case cfg.Role == "":
		return "this node has no role"
	case cfg.Role == store.RoleCheckpoint && !strings.EqualFold(cfg.HQCall, c.PeerCall):
		return "the HQ callsign changed"
	}
	return ""
}

// maxRun is the longest a run may take before it is abandoned (e.g. the
// service stalled mid-run): its probes, the reply wait, and slack.
func maxRun(c store.LinkCheck) time.Duration {
	return time.Duration(c.Count*c.SpacingSec)*time.Second + replyWait + time.Minute
}

func (s *Service) progress(ctx context.Context, cfg store.Settings, c store.LinkCheck, now time.Time) error {
	if reason := s.blocked(cfg, c); reason != "" {
		_, err := s.store.CancelLinkCheck(ctx, c.ID, reason)
		return err
	}
	if now.Sub(*c.StartedAt) > maxRun(c) {
		_, err := s.store.CancelLinkCheck(ctx, c.ID, "interrupted: the run stalled")
		return err
	}
	probes, err := s.store.ListLinkProbes(ctx, c.ID)
	if err != nil {
		return err
	}
	spacing := time.Duration(c.SpacingSec) * time.Second
	sent := len(probes)
	var lastSent time.Time
	if sent > 0 && probes[sent-1].SentAt != nil {
		lastSent = *probes[sent-1].SentAt
	}
	if sent < c.Count {
		// Keep the spacing from the actual last send, so a stall never
		// turns into a burst of probes.
		due := c.StartedAt.Add(time.Duration(sent) * spacing)
		if sent > 0 && lastSent.Add(spacing).After(due) {
			due = lastSent.Add(spacing)
		}
		if now.Before(due) {
			return nil
		}
		return s.sendProbe(ctx, cfg, c, sent+1, now)
	}
	allAcked := true
	for _, p := range probes {
		if p.GWMessageID != nil && p.AckedAt == nil {
			allAcked = false
		}
	}
	switch {
	case c.ReplyReceived && (allAcked || !now.Before(lastSent.Add(ackWait))),
		!now.Before(lastSent.Add(replyWait)):
		return s.finish(ctx, c, now)
	}
	return nil
}

// sendProbe sends probe idx. A failed send is recorded without a
// graywolf row, so the run keeps its timing and counts it as lost.
func (s *Service) sendProbe(ctx context.Context, cfg store.Settings, c store.LinkCheck, idx int, now time.Time) error {
	text, err := wire.EncodeProbe(wire.Probe{CP: c.StationCode, Run: uint16(c.Run), Index: uint8(idx), Total: uint8(c.Count)})
	if err != nil {
		_, cerr := s.store.CancelLinkCheck(ctx, c.ID, err.Error())
		return errors.Join(err, cerr)
	}
	s.ensure(ctx, c.PeerCall)
	p := store.LinkProbe{CheckID: c.ID, Idx: idx, SentAt: &now}
	msg, sendErr := s.gw.SendMessage(ctx, s.request(cfg, c.PeerCall, text))
	if sendErr == nil {
		p.GWMessageID, p.MsgID = &msg.ID, msg.MsgID
		s.recordRow(ctx, msg.ID, store.GWRowProbe)
	} else {
		s.log.Warn("linkcheck: send probe", "peer", c.PeerCall, "probe", idx, "err", sendErr)
		if err := s.store.NoteLinkCheckError(ctx, c.ID, "graywolf refused a probe: "+errText(sendErr)); err != nil {
			return err
		}
	}
	return s.store.SaveLinkProbe(ctx, p)
}

// onReply takes the responder's summary for the running check.
func (s *Service) onReply(ctx context.Context, sender string, q *wire.ProbeReply) error {
	c, err := s.store.ActiveLinkCheck(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(c.PeerCall, sender) || c.Run != int(q.Run) || c.StationCode != q.CP {
		return nil
	}
	heard := 0
	for _, i := range q.Heard {
		if int(i) <= c.Count {
			heard++
		}
	}
	var lvl *int
	if q.Level != wire.LevelUnknown {
		l := int(q.Level)
		lvl = &l
	}
	_, err = s.store.RecordLinkReply(ctx, c.ID, heard, lvl, q.Via)
	return err
}

// finish grades the run: it catches ACKs the feed missed, reads the
// local receive level of the peer's ACKs and reply, then records the
// result unless a reply arrived meanwhile (then the next tick redoes it).
func (s *Service) finish(ctx context.Context, c store.LinkCheck, now time.Time) error {
	probes, err := s.store.ListLinkProbes(ctx, c.ID)
	if err != nil {
		return err
	}
	for _, p := range probes {
		if p.GWMessageID == nil || p.AckedAt != nil {
			continue
		}
		if m, err := s.gw.GetMessage(ctx, *p.GWMessageID); err == nil && m.Status == graywolf.StatusAcked {
			if err := s.markProbeAcked(ctx, m, now); err != nil {
				return err
			}
		}
	}
	if probes, err = s.store.ListLinkProbes(ctx, c.ID); err != nil {
		return err
	}
	var rtts []int
	var msgIDs []string
	roundTrip := 0
	for _, p := range probes {
		if p.MsgID != "" {
			msgIDs = append(msgIDs, p.MsgID)
		}
		if p.AckedAt != nil {
			roundTrip++
			if p.RTTms != nil {
				rtts = append(rtts, *p.RTTms)
			}
		}
	}
	replyPrefix := wire.Prefix + "Q " + c.StationCode + " " + itoa(c.Run) + " "
	levels, _ := s.levelAt(ctx, c.PeerCall, c.StartedAt.Add(-time.Minute), func(pm *graywolf.PacketMessage) bool {
		return (pm.IsAck && slices.Contains(msgIDs, pm.MessageID)) || strings.HasPrefix(pm.Text, replyPrefix)
	})
	// Re-read the reply fields after the slow graywolf calls above.
	cur, err := s.store.GetLinkCheck(ctx, c.ID)
	if err != nil {
		return err
	}
	r := store.LinkResult{FinishedAt: now, RoundTrip: roundTrip, MedianRTTms: medianInt(rtts), LocalLevel: medianInt(levels)}
	m := Measure{N: cur.Count, Uplink: cur.Uplink, RoundTrip: roundTrip, Reply: cur.ReplyReceived}
	if r.MedianRTTms != nil {
		d := time.Duration(*r.MedianRTTms) * time.Millisecond
		m.MedianRTT = &d
	}
	r.Verdict = Verdict(m)
	r.Advice = Advice(r.Verdict, cur.RemoteLevel, r.LocalLevel)
	ok, err := s.store.FinishLinkCheck(ctx, c.ID, cur.ReplyReceived, r)
	if ok {
		s.log.Info("linkcheck: finished", "peer", c.PeerCall, "verdict", r.Verdict, "uplink", cur.Uplink, "round_trip", roundTrip, "reply", cur.ReplyReceived)
	}
	return err
}
