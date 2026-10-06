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
	s := New("K1CP")
	m, err := s.SendMessage(ctx, graywolf.SendRequest{To: "n0hq", Text: "RC1 H AS5 0 120000", ClientID: "c1"})
	if err != nil || m.ID != 1 || m.MsgID != "1" || m.ToCall != "N0HQ" || m.ClientID != "c1" {
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
	s := New("K1CP")
	boom := errors.New("boom")
	s.FailNextSend(boom)
	if _, err := s.SendMessage(ctx, graywolf.SendRequest{To: "N0HQ", Text: "x"}); !errors.Is(err, boom) {
		t.Fatalf("send err = %v", err)
	}
	m, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "N0HQ", Text: "x"})
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
	s := New("N0HQ")
	s.Now = func() time.Time { return now }
	s.Inbound("K1CP", "RC1 H AS5 0 120000")
	now = now.Add(time.Hour)
	out, _ := s.SendMessage(ctx, graywolf.SendRequest{To: "K1CP", Text: "RC1 G AS5 1"})
	_, _ = s.SendMessage(ctx, graywolf.SendRequest{To: "OTHER", Text: "hi"})

	var ids []uint64
	collect := func(ch graywolf.MessageChange) error { ids = append(ids, ch.ID); return nil }
	cursor, err := s.CatchUp(ctx, graywolf.ListParams{Folder: graywolf.FolderSent, Peer: "k1cp"}, collect)
	if err != nil || len(ids) != 1 || ids[0] != out.ID || cursor != "2" {
		t.Fatalf("sent to K1CP = %v, cursor %q, %v", ids, cursor, err)
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
	_, _ = s.CatchUp(ctx, graywolf.ListParams{Cursor: "2"}, collect)
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
	s := New("K1CP")
	p, err := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "n0hq")
	if err != nil || !p.WaitForAck || s.HasPrefsOverride(graywolf.ThreadKindDM, "N0HQ") {
		t.Fatalf("defaults = %+v, %v", p, err)
	}
	if _, err := s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "n0hq", graywolf.ConversationPrefs{WaitForAck: false}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ"); p.WaitForAck {
		t.Fatal("override not stored")
	}
	_, _ = s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ", graywolf.ConversationPrefs{WaitForAck: true})
	if s.HasPrefsOverride(graywolf.ThreadKindDM, "N0HQ") {
		t.Error("restoring defaults left an override")
	}
	s.FailPrefs(errors.New("down"))
	if _, err := s.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ"); err == nil {
		t.Error("expected error")
	}
	if _, err := s.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0HQ", graywolf.ConversationPrefs{}); err == nil {
		t.Error("expected error")
	}
}
