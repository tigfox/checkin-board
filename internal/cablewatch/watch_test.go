package cablewatch

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"checkin-board/internal/gwfake"
	"checkin-board/internal/hostmon"
	"checkin-board/internal/store"
)

var ctx = context.Background()

type fakeHost struct {
	mu   sync.Mutex
	snap hostmon.Snapshot
}

func (f *fakeHost) Cards() ([]hostmon.SoundCard, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap.SoundCards, f.snap.SoundCardsKnown
}
func (f *fakeHost) set(known bool, ids ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snap.SoundCardsKnown, f.snap.SoundCards = known, nil
	for i, id := range ids {
		f.snap.SoundCards = append(f.snap.SoundCards, hostmon.SoundCard{Index: i, ID: id})
	}
}

func settings(context.Context) (store.Settings, error) { return store.DefaultSettings(), nil }

func newWatcher(t *testing.T, host *fakeHost, local bool) (*Watcher, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	w := New(Config{Graywolf: gwfake.New("N0CALL-1"), Settings: settings, Cards: host.Cards, Local: local,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	return w, &logs
}

func TestWatcher(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "AllInOneCable") // gwfake's default channel uses CARD=AllInOneCable
	w, logs := newWatcher(t, host, true)
	w.Check(ctx)
	if m := w.Missing(); m != nil || w.HoldReason() != "" {
		t.Fatalf("present: missing %v, hold %q", m, w.HoldReason())
	}

	host.set(true, "vc4hdmi")
	w.Check(ctx)
	if m := w.Missing(); len(m) != 1 || m[0] != "AllInOneCable" || w.HoldReason() != "radio cable missing" {
		t.Fatalf("gone: missing %v, hold %q", m, w.HoldReason())
	}
	w.Check(ctx) // no second log line for the same state
	if n := strings.Count(logs.String(), "radio cable missing"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs)
	}

	host.set(false) // can't tell: the last reading stands
	w.Check(ctx)
	if w.HoldReason() == "" {
		t.Fatal("an unknown reading cleared the hold")
	}

	host.set(true, "AllInOneCable", "vc4hdmi")
	w.Check(ctx)
	if w.HoldReason() != "" || !strings.Contains(logs.String(), "radio cable back") {
		t.Fatalf("back: hold %q\n%s", w.HoldReason(), logs)
	}
}

// graywolf on another machine: this node's cards say nothing; never hold.
func TestWatcherRemoteGraywolf(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	w, _ := newWatcher(t, host, false)
	w.Check(ctx)
	if w.HoldReason() != "" || w.Missing() != nil {
		t.Fatal("remote graywolf must never hold")
	}
}

func TestWatcherSettingsError(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	w := New(Config{Graywolf: gwfake.New("N0CALL-1"), Cards: host.Cards, Local: true,
		Settings: func(context.Context) (store.Settings, error) { return store.Settings{}, errors.New("db") }})
	w.Check(ctx)
	if w.HoldReason() != "" {
		t.Fatal("no settings: no reading, no hold")
	}
}

func TestWatcherMissingIsACopy(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	w, _ := newWatcher(t, host, true)
	w.Check(ctx)
	w.Missing()[0] = "changed"
	if w.Missing()[0] != "AllInOneCable" {
		t.Fatal("Missing shares the watcher's slice")
	}
}

func TestRunChecksAtOnceAndStops(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	w, _ := newWatcher(t, host, true)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(runCtx, time.Hour) }()
	deadline := time.Now().Add(5 * time.Second)
	for w.HoldReason() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if w.HoldReason() == "" {
		t.Fatal("Run didn't check at start")
	}
}

// A hold nobody can confirm is released after StaleAfter.
func TestWatcherReleasesStaleHold(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	now := time.Date(2026, 10, 11, 21, 0, 0, 0, time.UTC)
	gw := gwfake.New("N0CALL-1")
	var logs bytes.Buffer
	w := New(Config{Graywolf: gw, Settings: settings, Cards: host.Cards, Local: true,
		Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	w.Check(ctx)
	if w.HoldReason() == "" {
		t.Fatal("no hold")
	}
	gw.SetAPIDown(true) // graywolf unreachable: can't tell
	now = now.Add(StaleAfter - time.Second)
	w.Check(ctx)
	if w.HoldReason() == "" {
		t.Fatal("released too early")
	}
	now = now.Add(time.Second)
	w.Check(ctx)
	if w.HoldReason() != "" || !strings.Contains(logs.String(), "unknown for too long") {
		t.Fatalf("stale hold kept: %q\n%s", w.HoldReason(), logs.String())
	}
}

// Changing the race channel to one whose card is present clears it.
func TestWatcherFollowsChannelSetting(t *testing.T) {
	host := &fakeHost{}
	host.set(true, "vc4hdmi")
	w, _ := newWatcher(t, host, true)
	w.Check(ctx)
	if w.HoldReason() == "" {
		t.Fatal("no hold")
	}
	w.cfg.Settings = func(context.Context) (store.Settings, error) {
		c := store.DefaultSettings()
		c.GWChannel = 99 // no such channel: nothing to check
		return c, nil
	}
	w.Check(ctx)
	if w.HoldReason() != "" {
		t.Fatal("hold kept for a channel that isn't the race's")
	}
}
