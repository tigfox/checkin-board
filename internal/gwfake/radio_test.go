package gwfake

import (
	"math"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
)

// The channel model itself: delivered/sent must match (1-loss)(1+dup).
func TestRadioLossAndDupRates(t *testing.T) {
	for _, loss := range []float64{0.1, 0.3, 0.5} {
		now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
		r := NewRadio(42, Profile{Loss: loss, Dup: 0.1}, func() time.Time { return now })
		const n = 20000
		for range n {
			r.Inject(Frame{From: "A", To: "B", Text: "x"})
		}
		r.Deliver()
		got := float64(r.Delivered()) / n
		want := (1 - loss) * 1.1
		if math.Abs(got-want) > 0.02 {
			t.Errorf("loss %.1f: delivered/sent = %.3f, want %.3f", loss, got, want)
		}
	}
}

func TestRadioDeliversDMsAndACKs(t *testing.T) {
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := NewRadio(1, Profile{}, clock)
	a, b := New("AAA"), New("BBB")
	a.Now, b.Now = clock, clock
	r.Attach(a)
	r.Attach(b)
	m, _ := a.SendMessage(ctx, graywolf.SendRequest{To: "BBB", Text: "RC1 H AS1 0 130000"})
	r.Deliver() // B hears the DM and ACKs
	if rows := b.Rows(); len(rows) != 1 || rows[0].Direction != "in" || rows[0].MsgID != m.MsgID {
		t.Fatalf("B rows = %+v", rows)
	}
	r.Deliver() // A hears the ACK
	if got, _ := a.GetMessage(ctx, m.ID); got.Status != graywolf.StatusAcked {
		t.Fatalf("A row = %+v, want acked", got)
	}
	// A resend inside the dedup window is ACKed again but not stored again.
	_, _ = a.ResendMessage(ctx, m.ID)
	r.Deliver()
	if n := len(b.Rows()); n != 1 {
		t.Fatalf("B stored a duplicate: %d rows", n)
	}
	sent := r.Sent()
	r.SetDown(true)
	_, _ = a.SendMessage(ctx, graywolf.SendRequest{To: "BBB", Text: "lost"})
	r.Deliver()
	if r.Sent() != sent+1 || len(b.Rows()) != 1 {
		t.Fatal("frame got through a down channel")
	}
	// After the dedup window the same frame is stored again.
	r.SetDown(false)
	now = now.Add(dedupWindow + time.Second)
	_, _ = a.ResendMessage(ctx, m.ID)
	r.Deliver()
	if n := len(b.Rows()); n != 2 {
		t.Fatalf("B rows = %d, want a new row after the dedup window", n)
	}
}

func TestPacketLogRecordsHeardFramesWithLevel(t *testing.T) {
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	r := NewRadio(1, Profile{}, clock)
	a, b := New("AAA"), New("BBB")
	a.Now, b.Now = clock, clock
	r.Attach(a)
	r.Attach(b)
	b.SetRXLevel("AAA", -18) // A's frames reach B's modem at -18 dBFS; B's reach A with no level (TNC)
	_, _ = a.SendMessage(ctx, graywolf.SendRequest{To: "BBB", Text: "RC1 P AS5 7 1/5"})
	r.Deliver() // B hears the DM, ACKs
	now = now.Add(time.Second)
	r.Deliver() // A hears the ACK

	pk, err := b.ListPackets(ctx, graywolf.PacketQuery{Type: "message", Direction: "RX"})
	if err != nil || len(pk) != 1 {
		t.Fatalf("B packets = %+v, %v", pk, err)
	}
	p := pk[0]
	if p.AudioLevel == nil || p.AudioLevel.LevelDBFS != -18 || p.Decoded.Source != "AAA" || p.Decoded.Message.Text != "RC1 P AS5 7 1/5" {
		t.Fatalf("B packet = %+v", p)
	}
	pa, _ := a.ListPackets(ctx, graywolf.PacketQuery{Direction: "RX"})
	if len(pa) != 1 || !pa[0].Decoded.Message.IsAck || pa[0].AudioLevel != nil {
		t.Fatalf("A packets = %+v", pa)
	}
	if late, _ := a.ListPackets(ctx, graywolf.PacketQuery{Since: now.Add(time.Second)}); len(late) != 0 {
		t.Fatalf("since filter: %+v", late)
	}
}
