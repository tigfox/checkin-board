package gwfake

import (
	"context"
	"errors"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
)

var ctx = context.Background()

func TestSendResendAndStatus(t *testing.T) {
	s := New("N0CALL-1")
	m, err := s.SendMessage(ctx, graywolf.SendRequest{To: "n0call-10", Text: "RC1 H AS5 0 120000", ClientID: "c1"})
	if err != nil || m.ID != 1 || m.MsgID != "1" || m.ToCall != "N0CALL-10" || m.ClientID != "c1" {
		t.Fatalf("send = %+v, %v", m, err)
	}
	if got, _ := s.GetMessage(ctx, m.ID); got.ClientID != "" {
		t.Error("client_id persisted without EchoClientID")
	}
	s.Ack(m.ID)
	r, err := s.ResendMessage(ctx, m.ID)
	if err != nil || r.Status != graywolf.StatusAcked || r.Attempts != 2 || r.MsgID != m.MsgID {
		t.Fatalf("resend = %+v, %v; want acked status and msgid kept", r, err)
	}
	if tx := s.Transmissions(); len(tx) != 2 || !tx[1].Resend {
		t.Fatalf("transmissions = %+v", tx)
	}
	s.SetInFlight(m.ID, true)
	if _, err := s.ResendMessage(ctx, m.ID); !graywolf.IsConflict(err) {
		t.Errorf("in-flight resend err = %v", err)
	}
	s.Delete(m.ID)
	if _, err := s.ResendMessage(ctx, m.ID); !graywolf.IsNotFound(err) {
		t.Errorf("deleted resend err = %v", err)
	}
	if _, err := s.GetMessage(ctx, m.ID); !graywolf.IsNotFound(err) {
		t.Errorf("deleted get err = %v", err)
	}
}

func TestInjectedFailures(t *testing.T) {
	s := New("N0CALL-1")
	boom := errors.New("boom")
	s.FailNextSend(boom)
	if _, err := s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("send err = %v", err)
	}
	m, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "x"})
	s.FailNextResend(boom)
	if _, err := s.ResendMessage(ctx, m.ID); !errors.Is(err, boom) {
		t.Fatalf("resend err = %v", err)
	}
	s.Reject(m.ID)
	if got, _ := s.GetMessage(ctx, m.ID); got.Status != graywolf.StatusRejected || got.AckedAt == nil {
		t.Fatalf("rejected = %+v", got)
	}
}

func TestCatchUpFilters(t *testing.T) {
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	s := New("N0CALL-10")
	s.Now = func() time.Time { return now }
	s.Inbound("N0CALL-1", "RC1 H AS5 0 120000")
	now = now.Add(time.Hour)
	out, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-1", Text: "RC1 G AS5 1"})
	_, _ = s.SendMessage(ctx, graywolf.SendRequest{To: "OTHER", Text: "hi"})

	var ids []uint64
	collect := func(ch graywolf.MessageChange) error { ids = append(ids, ch.ID); return nil }
	cursor, err := s.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderSent, Peer: "n0call-1"}, collect)
	if err != nil || len(ids) != 1 || ids[0] != out.ID || cursor == "" {
		t.Fatalf("sent to N0CALL-1 = %v, cursor %q, %v", ids, cursor, err)
	}
	ids = nil
	_, _ = s.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderInbox}, collect)
	if len(ids) != 1 || ids[0] != 1 {
		t.Fatalf("inbox = %v", ids)
	}
	ids = nil
	_, _ = s.CatchUp(ctx, graywolf.ListParams{Since: now}, collect)
	if len(ids) != 2 {
		t.Fatalf("since = %v, want the two later rows", ids)
	}
	ids = nil
	_, _ = s.CatchUp(ctx, graywolf.ListParams{Cursor: cursor}, collect)
	if len(ids) != 1 || ids[0] != 3 {
		t.Fatalf("after cursor = %v", ids)
	}
	stop := errors.New("stop")
	if c, err := s.CatchUp(ctx, graywolf.ListParams{}, func(graywolf.MessageChange) error { return stop }); !errors.Is(err, stop) || c != "" {
		t.Fatalf("handler error: cursor %q, %v", c, err)
	}
	if rows := s.Rows(); len(rows) != 3 || rows[0].ID != 1 {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestConversationPrefs(t *testing.T) {
	s := New("N0CALL-1")
	p, err := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "n0call-10")
	if err != nil || !p.WaitForAck || s.HasPrefsOverride(graywolf.ThreadKindDM, "N0CALL-10") {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	if _, err := s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "n0call-10", graywolf.ConversationPrefs{WaitForAck: false}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10"); p.WaitForAck {
		t.Fatal("override not stored")
	}
	_, _ = s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10", graywolf.ConversationPrefs{WaitForAck: true})
	if s.HasPrefsOverride(graywolf.ThreadKindDM, "N0CALL-10") {
		t.Error("restoring defaults left an override")
	}
	s.FailPrefs(errors.New("down"))
	if _, err := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10"); err == nil {
		t.Error("expected error")
	}
	if _, err := s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10", graywolf.ConversationPrefs{}); err == nil {
		t.Error("expected error")
	}
}

func TestFeedRelistsChangedRowsAndStreams(t *testing.T) {
	s := New("N0CALL-1")
	sent, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "RC1 H AS5 0 120000"})
	_, _ = s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "RC1 H AS5 0 120500"})
	var ids []uint64
	cursor, _ := s.CatchUp(ctx, graywolf.ListParams{}, func(ch graywolf.MessageChange) error { ids = append(ids, ch.ID); return nil })
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	opened := make(chan struct{})
	events := make(chan graywolf.Event, 4)
	go func() {
		_ = s.StreamEventsWithOpen(streamCtx, func() { close(opened) }, func(ev graywolf.Event) error { events <- ev; return nil })
	}()
	<-opened
	s.Ack(sent.ID)
	select {
	case ev := <-events:
		if ev.Type != graywolf.EventAcked || ev.Change.ID != sent.ID {
			t.Fatalf("event = %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no event for the ACK")
	}
	ids = nil
	_, _ = s.CatchUp(ctx, graywolf.ListParams{Cursor: cursor}, func(ch graywolf.MessageChange) error { ids = append(ids, ch.ID); return nil })
	if len(ids) != 1 || ids[0] != sent.ID {
		t.Fatalf("after ACK the feed listed %v, want the acked row again", ids)
	}

	in := s.Inbound("N0CALL-10", "RC1 G AS5 1")
	if err := s.MarkRead(ctx, in.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMessage(ctx, in.ID); got.Unread {
		t.Error("still unread")
	}
	if err := s.MarkRead(ctx, 999); !graywolf.IsNotFound(err) {
		t.Errorf("missing row err = %v", err)
	}
	s.SetMaxText(150)
	if p, _ := s.MessagePreferences(ctx); p.MaxText() != 150 {
		t.Errorf("MaxText = %d", p.MaxText())
	}
}

func TestDeleteMessage(t *testing.T) {
	s := New("N0CALL-1")
	m, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "N0CALL-10", Text: "x"})
	if err := s.DeleteMessage(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMessage(ctx, m.ID); !graywolf.IsNotFound(err) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestStationConfigFakes(t *testing.T) {
	s := New("N0CALL-1")
	if v, _ := s.Version(ctx); v.Version == "" {
		t.Fatal("version")
	}
	if c, _ := s.SetStationCallsign(ctx, "n0call-6"); c.Callsign != "N0CALL-6" {
		t.Fatalf("set = %+v", c)
	}
	if c, _ := s.StationConfig(ctx); c.Callsign != "N0CALL-6" {
		t.Fatalf("get = %+v", c)
	}
}

func TestAPIDownFailsCallsButRadioWorks(t *testing.T) {
	s := New("AAA")
	NewRadio(1, Profile{}, time.Now).Attach(s)
	s.SetAPIDown(true)
	if _, err := s.SendMessage(ctx, graywolf.SendRequest{To: "BBB", Text: "x"}); err == nil {
		t.Fatal("send while down")
	}
	if _, err := s.CatchUp(ctx, graywolf.ListParams{}, func(graywolf.MessageChange) error { return nil }); err == nil {
		t.Fatal("catch-up while down")
	}
	if _, err := s.ListPackets(ctx, graywolf.PacketQuery{}); err == nil {
		t.Fatal("packets while down")
	}
	s.hear(Frame{From: "BBB", To: "AAA", Text: "hi", MsgID: "1"}) // RF still received
	s.SetAPIDown(false)
	if rows := s.Rows(); len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
}
