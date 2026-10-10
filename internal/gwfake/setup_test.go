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
