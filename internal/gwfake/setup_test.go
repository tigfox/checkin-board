package gwfake

import (
	"testing"

	"checkin-board/internal/graywolf"
)

func TestRadioSetupReads(t *testing.T) {
	s := New("N0CALL-1")
	devs, _ := s.AudioDevices(ctx)
	chans, _ := s.Channels(ctx)
	lv, _ := s.AudioLevels(ctx)
	st, err := s.ChannelStats(ctx, 1)
	if len(devs) != 2 || len(chans) != 1 || !chans[0].PTT.Configured || lv[1].PeakDBFS != -45 || err != nil || st.RxFrames != 3 {
		t.Fatalf("default setup: %+v %+v %+v %+v %v", devs, chans, lv, st, err)
	}
	if _, err := s.ChannelStats(ctx, 9); !graywolf.IsNotFound(err) {
		t.Fatalf("unknown channel err = %v", err)
	}
	s.EditRadio(func(r *RadioSetup) { r.Commit = "4978244d-armv6buf" })
	if v, _ := s.Version(ctx); v.Commit != "4978244d-armv6buf" {
		t.Fatalf("version = %+v", v)
	}
	s.SetAPIDown(true)
	if _, err := s.Channels(ctx); err == nil {
		t.Fatal("Channels worked with the API down")
	}
	if _, err := s.AudioDevices(ctx); err == nil {
		t.Fatal("AudioDevices worked with the API down")
	}
	if _, err := s.AudioLevels(ctx); err == nil {
		t.Fatal("AudioLevels worked with the API down")
	}
	if _, err := s.ChannelStats(ctx, 1); err == nil {
		t.Fatal("ChannelStats worked with the API down")
	}
}

func TestConfigSettings(t *testing.T) {
	s := New("N0CALL-1")
	tts, _ := s.TxTimings(ctx)
	tt := tts[0]
	tt.TxDelayMS = 450
	if _, err := s.SetTxTiming(ctx, tt); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.TxTimings(ctx); got[0].TxDelayMS != 450 {
		t.Fatalf("tx timing = %+v", got)
	}
	if _, err := s.SetTxTiming(ctx, graywolf.TxTiming{ID: 9}); !graywolf.IsNotFound(err) {
		t.Fatalf("unknown tx timing err = %v", err)
	}
	d, _ := s.Digipeater(ctx)
	d.Enabled = true
	_, _ = s.SetDigipeater(ctx, d)
	if got, _ := s.Digipeater(ctx); !got.Enabled || got.ID != 1 {
		t.Fatalf("digipeater = %+v", got)
	}
	p, _ := s.MessagePreferences(ctx)
	p.RetentionDays = 30
	_, _ = s.SetMessagePreferences(ctx, p)
	if got, _ := s.MessagePreferences(ctx); got.RetentionDays != 30 {
		t.Fatalf("prefs = %+v", got)
	}
	s.SetAPIDown(true)
	for name, call := range map[string]func() error{
		"TxTimings":             func() error { _, err := s.TxTimings(ctx); return err },
		"SetTxTiming":           func() error { _, err := s.SetTxTiming(ctx, tt); return err },
		"Digipeater":            func() error { _, err := s.Digipeater(ctx); return err },
		"SetDigipeater":         func() error { _, err := s.SetDigipeater(ctx, d); return err },
		"SetMessagePreferences": func() error { _, err := s.SetMessagePreferences(ctx, p); return err },
	} {
		if call() == nil {
			t.Errorf("%s worked with the API down", name)
		}
	}
}
