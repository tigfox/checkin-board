// Package cablewatch keeps watch on the radio cable: whether the sound
// card graywolf's race channel uses is on this node. In the field test
// of 2026-10-11 an AIOC dropped off a Pi Zero's USB on every transmit,
// and graywolf went on reporting messages as sent. While the card is
// missing, the checkpoint engine holds its reports (nothing can go on
// air), and the keypad, panel and admin pages say so. Only a
// checkpoint's reports are held: HQ's traffic and link checks aren't.
package cablewatch

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"checkin-board/internal/hostmon"
	"checkin-board/internal/radiocheck"
	"checkin-board/internal/store"
)

const (
	// Every is how often Run checks.
	Every = 5 * time.Second
	// StaleAfter: with no definite reading for this long (graywolf
	// unreachable, the card list unreadable), the hold is released. A
	// hold nobody can confirm must not stop race traffic for good.
	StaleAfter = 2 * time.Minute
	// checkTimeout bounds one check's graywolf calls.
	checkTimeout  = 5 * time.Second
	reasonMissing = "radio cable missing"
)

// Config configures a Watcher.
type Config struct {
	Graywolf radiocheck.Graywolf
	Settings func(context.Context) (store.Settings, error)
	// Cards lists the node's sound cards now (hostmon.ReadSoundCards).
	Cards func() ([]hostmon.SoundCard, bool)
	// Local: graywolf runs on this machine. Otherwise this node's sound
	// cards say nothing about graywolf's, and the watcher never holds.
	Local  bool
	Now    func() time.Time // nil: time.Now
	Logger *slog.Logger
}

// Watcher caches the latest reading. Safe for concurrent use.
type Watcher struct {
	cfg     Config
	log     *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	missing []string
	sure    time.Time // the last definite reading
}

// New returns a watcher; call Run (or Check) to take readings.
func New(cfg Config) *Watcher {
	w := &Watcher{cfg: cfg, log: cfg.Logger, now: cfg.Now}
	if w.log == nil {
		w.log = slog.New(slog.DiscardHandler)
	}
	if w.now == nil {
		w.now = time.Now
	}
	return w
}

// Run checks now and then every interval until ctx ends.
func (w *Watcher) Run(ctx context.Context, every time.Duration) {
	w.Check(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check(ctx)
		}
	}
}

// Check takes one reading. One that can't tell (graywolf unreachable,
// cards unknown) keeps the previous reading, for up to StaleAfter.
func (w *Watcher) Check(ctx context.Context) {
	if !w.cfg.Local || w.cfg.Cards == nil || w.cfg.Settings == nil {
		return
	}
	missing, ok := w.read(ctx)
	now := w.now()
	w.mu.Lock()
	defer w.mu.Unlock()
	if !ok {
		if len(w.missing) > 0 && now.Sub(w.sure) >= StaleAfter {
			w.log.Warn("radio cable state unknown for too long; race reports resume", "cards", w.missing, "since", w.sure)
			w.missing = nil
		}
		return
	}
	switch {
	case len(missing) > 0 && !slices.Equal(missing, w.missing):
		w.log.Warn("radio cable missing: graywolf's sound card is not on this node; race reports are held", "cards", missing)
	case len(missing) == 0 && len(w.missing) > 0:
		w.log.Info("radio cable back; race reports resume", "cards", w.missing)
	}
	w.missing, w.sure = missing, now
}

func (w *Watcher) read(ctx context.Context) ([]string, bool) {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	cfg, err := w.cfg.Settings(ctx)
	if err != nil {
		return nil, false
	}
	cards, known := w.cfg.Cards()
	host := hostmon.Snapshot{SoundCards: cards, SoundCardsKnown: known}
	return radiocheck.MissingCards(ctx, w.cfg.Graywolf, cfg, host)
}

// Missing names the missing sound cards (nil: none, or not known).
func (w *Watcher) Missing() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.missing)
}

// HoldReason is why race traffic should wait ("": it needn't).
func (w *Watcher) HoldReason() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.missing) > 0 {
		return reasonMissing
	}
	return ""
}
