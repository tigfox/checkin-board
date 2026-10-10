package radiocheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/hostmon"
	"checkin-board/internal/store"
)

var ctx = context.Background()

// fakeGW is a healthy Pi Zero node: the fixed build, AIOC at 24 kHz.
type fakeGW struct {
	version  graywolf.Version
	devs     []graywolf.AudioDevice
	levels   map[uint32]graywolf.DeviceLevel
	chans    []graywolf.Channel
	stats    graywolf.ChannelStats
	versErr  error
	chansErr error
}

func healthy() *fakeGW {
	ch := graywolf.Channel{ID: 1, Name: "VHF APRS", Enabled: true, InputDeviceID: 1, OutputDeviceID: 2,
		PTT: graywolf.ChannelPTT{Method: "cm108", Configured: true}}
	ch.Backing.Health, ch.Backing.Modem.Active, ch.Backing.TX.Capable = "live", true, true
	return &fakeGW{
		version: graywolf.Version{Version: "0.14.14", Commit: "4978244d-armv6buf"},
		devs: []graywolf.AudioDevice{
			{ID: 1, Direction: "input", DevicePath: "plughw:CARD=AllInOneCable,DEV=0", SampleRate: 24000},
			{ID: 2, Direction: "output", DevicePath: "plughw:CARD=AllInOneCable,DEV=0", SampleRate: 24000},
		},
		levels: map[uint32]graywolf.DeviceLevel{1: {DeviceID: 1, PeakDBFS: -45}, 2: {DeviceID: 2, PeakDBFS: -60}},
		chans:  []graywolf.Channel{ch},
		stats:  graywolf.ChannelStats{Channel: 1, RxFrames: 5, RxBadFCS: 1, TxFrames: 2},
	}
}

func (f *fakeGW) Version(context.Context) (graywolf.Version, error) { return f.version, f.versErr }
func (f *fakeGW) AudioDevices(context.Context) ([]graywolf.AudioDevice, error) {
	return f.devs, nil
}
func (f *fakeGW) AudioLevels(context.Context) (map[uint32]graywolf.DeviceLevel, error) {
	return f.levels, nil
}
func (f *fakeGW) Channels(context.Context) ([]graywolf.Channel, error) { return f.chans, f.chansErr }
func (f *fakeGW) ChannelStats(_ context.Context, id uint32) (graywolf.ChannelStats, error) {
	return f.stats, nil
}

func piZero(keepingUp *bool) hostmon.Snapshot {
	return hostmon.Snapshot{Machine: "armv6l", ModemKeepingUp: keepingUp, ModemWaitsPerSec: 47}
}

func ptr[T any](v T) *T { return &v }

func cfg() store.Settings {
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall = store.RoleCheckpoint, "AS5", "KD2DCM-3"
	return c
}

func item(t *testing.T, r Report, key string) Item {
	t.Helper()
	for _, it := range r.Items {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("no %q item in %+v", key, r.Items)
	return Item{}
}

func TestHealthyPiZeroPasses(t *testing.T) {
	r := Check(ctx, healthy(), cfg(), piZero(ptr(true)))
	if r.Status != OK {
		t.Fatalf("report = %+v, want ok", r)
	}
	for _, it := range r.Items {
		if it.Status != OK {
			t.Errorf("%s = %s (%s), want ok", it.Key, it.Status, it.Detail)
		}
	}
}

func TestEachProblemIsFlagged(t *testing.T) {
	cases := map[string]struct {
		mut    func(*fakeGW, *store.Settings, *hostmon.Snapshot)
		key    string
		status Status
		detail string
	}{
		"stock build on a Pi Zero": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.version.Commit = "b589e686" },
			"build", Fail, "Pi Zero"},
		"graywolf unreachable": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.versErr = errors.New("connection refused") },
			"build", Fail, "reach"},
		"no channel": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.chans = nil },
			"channel", Fail, "no radio channel"},
		"configured channel missing": {func(_ *fakeGW, c *store.Settings, _ *hostmon.Snapshot) { c.GWChannel = 9 },
			"channel", Fail, "channel 9"},
		"modem down": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.chans[0].Backing.Health = "down" },
			"channel", Fail, "down"},
		"audio device missing": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.devs = f.devs[:1] },
			"audio", Fail, "output"},
		"no PTT": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.chans[0].PTT.Configured = false },
			"ptt", Fail, "key"},
		"48 kHz on a Pi Zero": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.devs[0].SampleRate = 48000 },
			"rate", Fail, "24 kHz"},
		"silent input": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) {
			f.levels[1] = graywolf.DeviceLevel{DeviceID: 1, PeakDBFS: -60}
		}, "level", Warn, "no audio"},
		"clipping input": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) {
			f.levels[1] = graywolf.DeviceLevel{DeviceID: 1, PeakDBFS: 0, Clipping: true}
		}, "level", Warn, "clipping"},
		"nothing decoded": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) { f.stats = graywolf.ChannelStats{} },
			"heard", Warn, "nothing"},
		"mostly damaged frames": {func(f *fakeGW, _ *store.Settings, _ *hostmon.Snapshot) {
			f.stats = graywolf.ChannelStats{RxFrames: 1, RxBadFCS: 6}
		}, "heard", Warn, "damaged"},
		"modem behind": {func(_ *fakeGW, _ *store.Settings, h *hostmon.Snapshot) { h.ModemKeepingUp = ptr(false) },
			"keepup", Fail, "behind"},
		"keep-up not measured": {func(_ *fakeGW, _ *store.Settings, h *hostmon.Snapshot) { h.ModemKeepingUp = nil },
			"keepup", Unknown, "not measured"},
	}
	for name, c := range cases {
		f, set, host := healthy(), cfg(), piZero(ptr(true))
		c.mut(f, &set, &host)
		r := Check(ctx, f, set, host)
		it := item(t, r, c.key)
		if it.Status != c.status || !strings.Contains(it.Detail, c.detail) {
			t.Errorf("%s: %s = %s %q, want %s containing %q", name, c.key, it.Status, it.Detail, c.status, c.detail)
		}
		if c.status == Fail && r.Status != Fail {
			t.Errorf("%s: overall %s, want fail", name, r.Status)
		}
	}
}

// A faster Pi (not armv6l) needs neither the patched build nor 24 kHz.
func TestFasterPiDoesNotNeedPiZeroFixes(t *testing.T) {
	f := healthy()
	f.version.Commit = "b589e686"
	f.devs[0].SampleRate, f.devs[1].SampleRate = 48000, 48000
	r := Check(ctx, f, cfg(), hostmon.Snapshot{Machine: "aarch64", ModemKeepingUp: ptr(true)})
	if it := item(t, r, "build"); it.Status != OK {
		t.Errorf("build = %+v", it)
	}
	if it := item(t, r, "rate"); it.Status != OK {
		t.Errorf("rate = %+v", it)
	}
}

func TestChannelErrorStopsDependentChecks(t *testing.T) {
	f := healthy()
	f.chansErr = errors.New("timeout")
	r := Check(ctx, f, cfg(), piZero(ptr(true)))
	if it := item(t, r, "channel"); it.Status != Fail {
		t.Fatalf("channel = %+v", it)
	}
	for _, k := range []string{"audio", "ptt", "rate", "level", "heard"} {
		if it := item(t, r, k); it.Status != Unknown {
			t.Errorf("%s = %+v, want unknown when channels can't be read", k, it)
		}
	}
}

// Decode quality is judged on what changed since the previous check, so
// hours of good traffic can't hide a receive path that just broke.
func TestHeardUsesChangeSinceLastCheck(t *testing.T) {
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	c := NewChecker(func() time.Time { return now })
	f := healthy()
	f.stats = graywolf.ChannelStats{RxFrames: 500, RxBadFCS: 4}
	if it := item(t, c.Check(ctx, f, cfg(), piZero(ptr(true))), "heard"); it.Status != OK {
		t.Fatalf("first check = %+v", it)
	}
	now = now.Add(10 * time.Minute)
	f.stats = graywolf.ChannelStats{RxFrames: 501, RxBadFCS: 12}
	it := item(t, c.Check(ctx, f, cfg(), piZero(ptr(true))), "heard")
	if it.Status != Warn || !strings.Contains(it.Detail, "since the last check") || !strings.Contains(it.Detail, "8 damaged") {
		t.Fatalf("second check = %+v, want a warning about the last 10 minutes", it)
	}
	// graywolf restarted (counters went back): judge the new totals.
	now = now.Add(time.Minute)
	f.stats = graywolf.ChannelStats{RxFrames: 2}
	if it := item(t, c.Check(ctx, f, cfg(), piZero(ptr(true))), "heard"); it.Status != OK || strings.Contains(it.Detail, "since the last check") {
		t.Fatalf("after a graywolf restart = %+v", it)
	}
}

// A channel backed by a KISS TNC has no audio devices to check.
func TestKISSChannelSkipsAudioItems(t *testing.T) {
	f := healthy()
	f.chans[0].Backing.Modem.Active = false
	f.chans[0].InputDeviceID, f.chans[0].OutputDeviceID = 0, 0
	r := Check(ctx, f, cfg(), piZero(ptr(true)))
	for _, k := range []string{"audio", "rate", "level"} {
		if it := item(t, r, k); it.Status != OK || !strings.Contains(it.Detail, "KISS") {
			t.Errorf("%s = %+v, want not applicable to a KISS TNC", k, it)
		}
	}
}

// Like the engines, a channel setting of 0 or less means automatic.
func TestNonPositiveChannelIsAutomatic(t *testing.T) {
	set := cfg()
	set.GWChannel = -1
	if it := item(t, Check(ctx, healthy(), set, piZero(ptr(true))), "channel"); it.Status != OK {
		t.Fatalf("channel = %+v", it)
	}
}

// The channel's devices missing: the rate can't be judged (not OK).
func TestRateUnknownWithoutDevices(t *testing.T) {
	f := healthy()
	f.devs = nil
	if it := item(t, Check(ctx, f, cfg(), piZero(ptr(true))), "rate"); it.Status != Unknown {
		t.Fatalf("rate = %+v, want unknown", it)
	}
}
