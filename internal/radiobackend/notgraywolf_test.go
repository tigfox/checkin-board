package radiobackend

import (
	"context"
	"errors"
	"testing"

	"checkin-board/internal/graywolf"
)

func TestGraywolfOnlyCalls(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	if v, err := h.b.Version(ctx); err != nil || v.Platform != "uvk5" || v.Version == "" {
		t.Fatalf("version = %+v %v", v, err)
	}
	for name, call := range map[string]func() error{
		"audio devices": func() error { _, err := h.b.AudioDevices(ctx); return err },
		"levels":        func() error { _, err := h.b.AudioLevels(ctx); return err },
		"channels":      func() error { _, err := h.b.Channels(ctx); return err },
		"stats":         func() error { _, err := h.b.ChannelStats(ctx, 1); return err },
		"tx timings":    func() error { _, err := h.b.TxTimings(ctx); return err },
		"set tx timing": func() error { _, err := h.b.SetTxTiming(ctx, graywolf.TxTiming{}); return err },
		"digipeater":    func() error { _, err := h.b.Digipeater(ctx); return err },
		"set digi":      func() error { _, err := h.b.SetDigipeater(ctx, graywolf.Digipeater{}); return err },
		"set prefs":     func() error { _, err := h.b.SetMessagePreferences(ctx, graywolf.MessagePreferences{}); return err },
	} {
		var ae *graywolf.APIError
		if err := call(); !errors.As(err, &ae) || ae.StatusCode != 501 {
			t.Errorf("%s: %v", name, err)
		}
	}
}
