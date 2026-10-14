package radiobackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
)

// Lending the radio (UV Studio, the flasher): the session closes, the
// backend doesn't reconnect, messages wait, and it comes back after.
func TestLend(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	giveBack, err := h.b.Lend(ctx, "UV Studio")
	if err != nil {
		t.Fatal(err)
	}
	st := h.b.State()
	if st.Connected || st.Lent != "UV Studio" || !strings.Contains(st.Error, "radio in use: UV Studio") {
		t.Fatalf("state while lent = %+v", st)
	}
	if _, err := h.b.Lend(ctx, "flasher"); !errors.Is(err, ErrLent) || !strings.Contains(err.Error(), "UV Studio") {
		t.Fatalf("second lend: %v", err)
	}
	h.mu.Lock()
	dials := h.dials
	h.mu.Unlock()
	m := h.send("waits while lent")
	time.Sleep(3 * minBackoff / 2) // past a reconnect attempt
	h.mu.Lock()
	again := h.dials
	h.mu.Unlock()
	if again != dials || h.status(m.ID) != graywolf.StatusQueued {
		t.Fatalf("while lent: %d dials (was %d), status %s", again, dials, h.status(m.ID))
	}
	giveBack()
	giveBack() // twice is harmless
	h.waitFor("reconnected", func() bool { return h.b.State().Connected })
	if h.b.State().Lent != "" {
		t.Fatal("still lent")
	}
	h.waitFor("sent after", func() bool { return h.status(m.ID) == statusSubmitted })
	// It can be lent again.
	gb, err := h.b.Lend(ctx, "flasher")
	if err != nil {
		t.Fatal(err)
	}
	gb()
}

// The radio's monitor lines stop (as after a timed-out write to the
// host): a report goes overdue, the backend turns them back on, and the
// next message's SENT arrives. Then a routine refresh every reviveEvery.
func TestReviveSilencedLines(t *testing.T) {
	h := newHarness(t)
	h.radio.Silence()
	m := h.send("lost line")
	h.waitFor("submitted", func() bool { return h.status(m.ID) == statusSubmitted })
	h.radio.Tick() // on air, but its SENT line is lost
	if h.status(m.ID) != statusSubmitted {
		t.Fatal("a SENT line got through while silenced")
	}
	if h.radio.Revivals() != 0 {
		t.Fatalf("revived too early: %d", h.radio.Revivals())
	}
	h.clk.Add(reviveOverdue + time.Second)
	h.waitFor("revived", func() bool { return h.radio.Revivals() == 1 })
	m2 := h.send("after the revival")
	h.waitFor("submitted", func() bool { return h.status(m2.ID) == statusSubmitted })
	h.clk.Add(10 * time.Second) // past the radio's gap
	h.radio.Tick()
	h.waitFor("SENT heard", func() bool { return h.status(m2.ID) == graywolf.StatusSentRF })
	// Nothing overdue: the next "monitor on" is the routine one.
	time.Sleep(3 * senderTick)
	if n := h.radio.Revivals(); n != 1 {
		t.Fatalf("%d revivals without cause", n)
	}
	h.clk.Add(reviveEvery)
	h.waitFor("routine refresh", func() bool { return h.radio.Revivals() == 2 })
}
