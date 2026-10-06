package store

import (
	"errors"
	"testing"
	"time"
)

func TestLinkCheckOneActiveAtATime(t *testing.T) {
	s := newTestStore(t)
	c, err := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, Source: "admin", RequestedAt: t0})
	if err != nil || c.ID == 0 || c.State != LinkCheckRequested {
		t.Fatalf("create = %+v, %v", c, err)
	}
	if _, err := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0}); !errors.Is(err, ErrConflict) {
		t.Fatalf("second active check: %v, want ErrConflict", err)
	}
	next, err := s.NextRequestedLinkCheck(ctx)
	if err != nil || next.ID != c.ID {
		t.Fatalf("next = %+v, %v", next, err)
	}
	started := at(time.Second)
	next.State, next.StartedAt, next.Run = LinkCheckRunning, &started, 42
	if err := s.UpdateLinkCheck(ctx, next); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NextRequestedLinkCheck(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("running check still requested: %v", err)
	}
	act, err := s.ActiveLinkCheck(ctx)
	if err != nil || act.ID != c.ID || act.Run != 42 {
		t.Fatalf("active = %+v, %v", act, err)
	}
	fin := at(2 * time.Minute)
	rtt := 3200
	act.State, act.FinishedAt, act.Verdict, act.Uplink, act.MedianRTTms = LinkCheckDone, &fin, "PASS", 5, &rtt
	if err := s.UpdateLinkCheck(ctx, act); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 3, SpacingSec: 10, RequestedAt: fin}); err != nil {
		t.Fatalf("new check after the first finished: %v", err)
	}
	list, err := s.ListLinkChecks(ctx, 10)
	if err != nil || len(list) != 2 || list[0].Count != 3 || list[1].Verdict != "PASS" || *list[1].MedianRTTms != 3200 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	got, err := s.GetLinkCheck(ctx, c.ID)
	if err != nil || got.FinishedAt == nil || !got.FinishedAt.Equal(fin) {
		t.Fatalf("get = %+v, %v", got, err)
	}
}

func TestLinkCheckValidates(t *testing.T) {
	s := newTestStore(t)
	for _, c := range []LinkCheck{
		{PeerCall: "", StationCode: "AS5", Count: 5, SpacingSec: 10},
		{PeerCall: "N0CALL-10", StationCode: "as 5", Count: 5, SpacingSec: 10},
		{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 0, SpacingSec: 10},
		{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 21, SpacingSec: 10},
		{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 0},
	} {
		if _, err := s.CreateLinkCheck(ctx, c); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%+v: err = %v, want ErrInvalidInput", c, err)
		}
	}
}

func TestLinkProbesAndLookupByMessage(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 2, SpacingSec: 10, RequestedAt: t0})
	id := uint64(77)
	sent := at(0)
	if err := s.SaveLinkProbe(ctx, LinkProbe{CheckID: c.ID, Idx: 1, GWMessageID: &id, SentAt: &sent}); err != nil {
		t.Fatal(err)
	}
	acked := at(4 * time.Second)
	rtt := 4000
	if err := s.SaveLinkProbe(ctx, LinkProbe{CheckID: c.ID, Idx: 1, GWMessageID: &id, SentAt: &sent, AckedAt: &acked, RTTms: &rtt}); err != nil {
		t.Fatal(err)
	}
	p, err := s.LinkProbeByMessage(ctx, 77)
	if err != nil || p.Idx != 1 || p.RTTms == nil || *p.RTTms != 4000 {
		t.Fatalf("probe = %+v, %v", p, err)
	}
	ps, err := s.ListLinkProbes(ctx, c.ID)
	if err != nil || len(ps) != 1 {
		t.Fatalf("probes = %+v, %v", ps, err)
	}
	if _, err := s.LinkProbeByMessage(ctx, 78); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown probe: %v", err)
	}
}

func TestLinkResponseRecordsHeardProbes(t *testing.T) {
	s := newTestStore(t)
	lvl := -20
	r, err := s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 5, Idx: 2, At: t0, Level: &lvl, Via: "WIDE1"})
	if err != nil || r.Heard != "2" || r.ID == 0 {
		t.Fatalf("first = %+v, %v", r, err)
	}
	r, err = s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 5, Idx: 1, At: at(10 * time.Second)})
	if err != nil || r.Heard != "1,2" || !r.LastHeardAt.Equal(at(10*time.Second)) || r.Level == nil || *r.Level != -20 || r.Via != "WIDE1" {
		t.Fatalf("second = %+v, %v", r, err)
	}
	// A repeat (duplicate frame) changes nothing.
	r, _ = s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 5, Idx: 2, At: at(11 * time.Second)})
	if r.Heard != "1,2" {
		t.Fatalf("repeat changed heard: %+v", r)
	}
	due := at(40 * time.Second)
	r.ReplyDueAt = &due
	if err := s.UpdateLinkResponse(ctx, r); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.DueLinkResponses(ctx, at(39*time.Second)); len(d) != 0 {
		t.Fatalf("due early: %+v", d)
	}
	d, err := s.DueLinkResponses(ctx, due)
	if err != nil || len(d) != 1 {
		t.Fatalf("due = %+v, %v", d, err)
	}
	gw := uint64(9)
	d[0].ReplyGWID, d[0].ReplyAttempts, d[0].ReplyDueAt, d[0].ReplySentAt = &gw, 1, nil, &due
	if err := s.UpdateLinkResponse(ctx, d[0]); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LinkResponseByReply(ctx, 9); err != nil || got.ID != r.ID {
		t.Fatalf("by reply = %+v, %v", got, err)
	}
	if n, _ := s.CountLinkSendsSince(ctx, t0); n != 1 {
		t.Fatalf("reply count = %d", n)
	}
	list, _ := s.ListLinkResponses(ctx, 10)
	if len(list) != 1 {
		t.Fatalf("responses = %+v", list)
	}
}

func TestResetClearsLinkChecks(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0})
	_, _ = s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 5, Idx: 1, At: t0})
	if err := s.ResetRaceData(ctx, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.ListLinkChecks(ctx, 10); len(l) != 0 {
		t.Fatalf("checks after reset: %+v", l)
	}
	if l, _ := s.ListLinkResponses(ctx, 10); len(l) != 0 {
		t.Fatalf("responses after reset: %+v", l)
	}
}

func TestCancelLinkCheckOnlyWhileActive(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0})
	if ok, err := s.CancelLinkCheck(ctx, c.ID, "no service"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := s.CancelLinkCheck(ctx, c.ID, "again"); ok {
		t.Fatal("cancelled twice")
	}
	got, _ := s.GetLinkCheck(ctx, c.ID)
	if got.State != LinkCheckCancelled || got.Error != "no service" || got.FinishedAt == nil {
		t.Fatalf("check = %+v", got)
	}
}

func TestPendingLinkCheck(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.PendingLinkCheck(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty: %v", err)
	}
	c, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0})
	if p, err := s.PendingLinkCheck(ctx); err != nil || p.ID != c.ID {
		t.Fatalf("pending = %+v, %v", p, err)
	}
}

func TestLinkCheckGuardedTransitions(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0})
	// A cancel that lands first wins: start no longer applies.
	if ok, _ := s.CancelLinkCheck(ctx, c.ID, "gave up"); !ok {
		t.Fatal("cancel")
	}
	if ok, err := s.StartLinkCheck(ctx, c.ID, 42, t0); ok || err != nil {
		t.Fatalf("start after cancel = %v, %v", ok, err)
	}
	c2, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 5, SpacingSec: 10, RequestedAt: t0})
	if ok, err := s.StartLinkCheck(ctx, c2.ID, 42, t0); !ok || err != nil {
		t.Fatalf("start = %v, %v", ok, err)
	}
	lvl := -20
	if ok, _ := s.RecordLinkReply(ctx, c2.ID, 4, &lvl, "WIDE1"); !ok {
		t.Fatal("reply")
	}
	if ok, _ := s.RecordLinkReply(ctx, c2.ID, 5, nil, ""); ok {
		t.Fatal("second reply accepted")
	}
	// A finish computed before the reply arrived must not overwrite it.
	fin := at(time.Minute)
	stale := LinkResult{FinishedAt: fin, Verdict: "FAIL", RoundTrip: 0}
	if ok, _ := s.FinishLinkCheck(ctx, c2.ID, false, stale); ok {
		t.Fatal("finish with a stale reply flag applied")
	}
	if ok, _ := s.FinishLinkCheck(ctx, c2.ID, true, LinkResult{FinishedAt: fin, Verdict: "PASS", RoundTrip: 5}); !ok {
		t.Fatal("finish")
	}
	got, _ := s.GetLinkCheck(ctx, c2.ID)
	if got.State != LinkCheckDone || got.Verdict != "PASS" || got.Uplink != 4 || !got.ReplyReceived || *got.RemoteLevel != -20 {
		t.Fatalf("finished = %+v", got)
	}
	if ok, _ := s.CancelLinkCheck(ctx, c2.ID, "late"); ok {
		t.Fatal("cancelled a finished run")
	}
	if err := s.NoteLinkCheckError(ctx, c2.ID, "x"); err != nil {
		t.Fatal(err)
	}
}

func TestLinkProbeAckOnce(t *testing.T) {
	s := newTestStore(t)
	c, _ := s.CreateLinkCheck(ctx, LinkCheck{PeerCall: "N0CALL-10", StationCode: "AS5", Count: 1, SpacingSec: 10, RequestedAt: t0})
	id := uint64(5)
	if err := s.SaveLinkProbe(ctx, LinkProbe{CheckID: c.ID, Idx: 1, GWMessageID: &id, MsgID: "17", SentAt: &t0}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.AckLinkProbe(ctx, 5, at(3*time.Second), 3000); !ok {
		t.Fatal("ack")
	}
	if ok, _ := s.AckLinkProbe(ctx, 5, at(9*time.Second), 9000); ok {
		t.Fatal("acked twice")
	}
	ps, _ := s.ListLinkProbes(ctx, c.ID)
	if *ps[0].RTTms != 3000 || ps[0].MsgID != "17" {
		t.Fatalf("probe = %+v", ps[0])
	}
}

func TestLinkReplyGuardedTransitions(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 2, Idx: 2, At: t0})
	if ok, _ := s.ScheduleLinkReply(ctx, r.ID, t0); !ok {
		t.Fatal("schedule")
	}
	gw := uint64(9)
	if ok, _ := s.MarkLinkReplySent(ctx, r.ID, gw, t0, at(30*time.Second)); !ok {
		t.Fatal("sent")
	}
	// Once sent, a late probe can't reschedule a fresh reply.
	if ok, _ := s.ScheduleLinkReply(ctx, r.ID, at(time.Second)); ok {
		t.Fatal("rescheduled after sending")
	}
	if n, _ := s.CountLinkSendsSince(ctx, t0); n != 1 {
		t.Fatalf("sends = %d", n)
	}
	if ok, _ := s.AckLinkReply(ctx, gw, at(2*time.Second)); !ok {
		t.Fatal("ack")
	}
	// A resend computed before the ACK must not undo it.
	if ok, _ := s.MarkLinkReplySent(ctx, r.ID, gw, at(30*time.Second), at(60*time.Second)); ok {
		t.Fatal("resend after ack recorded")
	}
	got, _ := s.LinkResponseByReply(ctx, gw)
	if got.ReplyAckedAt == nil || got.ReplyDueAt != nil || got.ReplyAttempts != 1 {
		t.Fatalf("response = %+v", got)
	}
	if err := s.StopLinkReply(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.RescheduleLinkReply(ctx, r.ID, at(time.Minute), true); ok {
		t.Fatal("rescheduled an acked reply")
	}
}

func TestProbeHeardLongAfterStartsFreshRun(t *testing.T) {
	s := newTestStore(t)
	r, _ := s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 2, Idx: 1, At: t0})
	_, _ = s.MarkLinkReplySent(ctx, r.ID, 9, t0, at(30*time.Second))
	// Same run number from the same station a day later: a new run.
	r2, err := s.RecordProbeHeard(ctx, ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 7, Total: 3, Idx: 2, At: at(24 * time.Hour)})
	if err != nil || r2.Heard != "2" || r2.Total != 3 || r2.ReplySentAt != nil || r2.ReplyAttempts != 0 {
		t.Fatalf("fresh = %+v, %v", r2, err)
	}
}
