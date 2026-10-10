package graywolf

import (
	"context"
	"net/http"
	"testing"
)

// Payloads as graywolf 0.14.14 returned them on a test node (2026-10-09).
const (
	audioDevicesJSON = `[
  {"id": 1, "name": "All-In-One-Cable", "direction": "input", "source_type": "soundcard",
   "device_path": "plughw:CARD=AllInOneCable,DEV=0", "sample_rate": 24000, "channels": 1, "format": "s16le", "gain_db": 0},
  {"id": 2, "name": "All-In-One-Cable", "direction": "output", "source_type": "soundcard",
   "device_path": "plughw:CARD=AllInOneCable,DEV=0", "sample_rate": 24000, "channels": 1, "format": "s16le", "gain_db": -1}]`
	audioLevelsJSON = `{"1": {"device_id": 1, "peak_dbfs": -45.398746, "rms_dbfs": -47.20494, "clipping": false},
  "2": {"device_id": 2, "peak_dbfs": -60, "rms_dbfs": -60, "clipping": false}}`
	channelsJSON = `[{"id": 1,
  "backing": {"modem": {"active": true}, "kiss_tnc": [], "summary": "modem", "health": "live", "tx": {"capable": true}},
  "ptt": {"method": "cm108", "configured": true, "detail": "GPIO 3 · /dev/hidraw0", "gpio_pin": 3},
  "name": "VHF APRS", "mode": "aprs", "input_device_id": 1, "input_channel": 0, "output_device_id": 2,
  "output_channel": 0, "modem_type": "afsk", "bit_rate": 1200, "mark_freq": 1200, "space_freq": 2200, "enabled": true}]`
	channelStatsJSON = `{"channel": 1, "rx_frames": 5, "rx_bad_fcs": 1, "tx_frames": 2, "dcd_transitions": 22,
  "audio_level_mark": 0, "audio_level_space": 0, "audio_level_peak": 0.0053710938, "dcd_state": false}`
	versionJSON = `{"version": "0.14.14", "commit": "4978244d-armv6buf", "platform": "linux"}`
)

func TestRadioSetupReads(t *testing.T) {
	f := newFakeGW(t)
	serve := func(pattern, body string) {
		f.mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	}
	serve("GET /api/audio-devices", audioDevicesJSON)
	serve("GET /api/audio-devices/levels", audioLevelsJSON)
	serve("GET /api/channels", channelsJSON)
	serve("GET /api/channels/1/stats", channelStatsJSON)
	serve("GET /api/version", versionJSON)
	c := f.client(t)
	ctx := context.Background()

	devs, err := c.AudioDevices(ctx)
	if err != nil || len(devs) != 2 || devs[1].Direction != "output" || devs[1].SampleRate != 24000 || devs[0].DevicePath != "plughw:CARD=AllInOneCable,DEV=0" {
		t.Fatalf("AudioDevices = %+v, %v", devs, err)
	}
	lv, err := c.AudioLevels(ctx)
	if err != nil || len(lv) != 2 || lv[1].PeakDBFS > -45 || lv[2].PeakDBFS != -60 {
		t.Fatalf("AudioLevels = %+v, %v", lv, err)
	}
	chs, err := c.Channels(ctx)
	if err != nil || len(chs) != 1 {
		t.Fatalf("Channels = %+v, %v", chs, err)
	}
	ch := chs[0]
	if !ch.Enabled || ch.InputDeviceID != 1 || ch.OutputDeviceID != 2 || !ch.PTT.Configured || ch.PTT.Method != "cm108" ||
		ch.Backing.Health != "live" || !ch.Backing.TX.Capable || ch.ModemType != "afsk" {
		t.Fatalf("channel = %+v", ch)
	}
	st, err := c.ChannelStats(ctx, 1)
	if err != nil || st.RxFrames != 5 || st.RxBadFCS != 1 || st.TxFrames != 2 {
		t.Fatalf("ChannelStats = %+v, %v", st, err)
	}
	v, err := c.Version(ctx)
	if err != nil || v.Commit != "4978244d-armv6buf" {
		t.Fatalf("Version = %+v, %v", v, err)
	}
}
