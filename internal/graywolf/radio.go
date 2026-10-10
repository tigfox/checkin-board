package graywolf

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
)

// Radio setup, read-only: what the radio status check (feedback
// 2026-10-09, item 6) needs from graywolf's API.

// AudioDevice is one entry of GET /api/audio-devices.
type AudioDevice struct {
	ID         uint32  `json:"id"`
	Name       string  `json:"name"`
	Direction  string  `json:"direction"` // "input" or "output"
	SourceType string  `json:"source_type"`
	DevicePath string  `json:"device_path"`
	SampleRate int     `json:"sample_rate"`
	Channels   int     `json:"channels"`
	Format     string  `json:"format"`
	GainDB     float64 `json:"gain_db"`
}

// DeviceLevel is one device's current level from GET /api/audio-devices/levels.
type DeviceLevel struct {
	DeviceID uint32  `json:"device_id"`
	PeakDBFS float64 `json:"peak_dbfs"`
	RMSDBFS  float64 `json:"rms_dbfs"`
	Clipping bool    `json:"clipping"`
}

// ChannelPTT is a channel's push-to-talk summary.
type ChannelPTT struct {
	Method     string `json:"method"`
	Configured bool   `json:"configured"`
	Detail     string `json:"detail"`
}

// ChannelBacking is what drives a channel (graywolf's modem or a KISS TNC).
type ChannelBacking struct {
	Summary string `json:"summary"`
	Health  string `json:"health"` // "live" when working
	Modem   struct {
		Active bool `json:"active"`
	} `json:"modem"`
	TX struct {
		Capable bool `json:"capable"`
	} `json:"tx"`
}

// Channel is one entry of GET /api/channels.
type Channel struct {
	ID             uint32         `json:"id"`
	Name           string         `json:"name"`
	Mode           string         `json:"mode"`
	Enabled        bool           `json:"enabled"`
	InputDeviceID  uint32         `json:"input_device_id"`
	OutputDeviceID uint32         `json:"output_device_id"`
	ModemType      string         `json:"modem_type"`
	BitRate        int            `json:"bit_rate"`
	PTT            ChannelPTT     `json:"ptt"`
	Backing        ChannelBacking `json:"backing"`
}

// ChannelStats is GET /api/channels/{id}/stats: counts since graywolf
// started.
type ChannelStats struct {
	Channel        uint32  `json:"channel"`
	RxFrames       uint64  `json:"rx_frames"`
	RxBadFCS       uint64  `json:"rx_bad_fcs"`
	TxFrames       uint64  `json:"tx_frames"`
	DCDTransitions uint64  `json:"dcd_transitions"`
	AudioLevelPeak float64 `json:"audio_level_peak"`
	DCDState       bool    `json:"dcd_state"`
}

// AudioDevices calls GET /api/audio-devices.
func (c *Client) AudioDevices(ctx context.Context) ([]AudioDevice, error) {
	var out []AudioDevice
	err := c.do(ctx, http.MethodGet, "/api/audio-devices", nil, nil, &out)
	return out, err
}

// AudioLevels calls GET /api/audio-devices/levels, keyed by device id.
func (c *Client) AudioLevels(ctx context.Context) (map[uint32]DeviceLevel, error) {
	var raw map[string]DeviceLevel
	if err := c.do(ctx, http.MethodGet, "/api/audio-devices/levels", nil, nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[uint32]DeviceLevel, len(raw))
	for k, v := range raw {
		id, err := strconv.ParseUint(k, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("graywolf: audio level key %q: %w", k, err)
		}
		out[uint32(id)] = v
	}
	return out, nil
}

// Channels calls GET /api/channels.
func (c *Client) Channels(ctx context.Context) ([]Channel, error) {
	var out []Channel
	err := c.do(ctx, http.MethodGet, "/api/channels", nil, nil, &out)
	return out, err
}

// ChannelStats calls GET /api/channels/{id}/stats.
func (c *Client) ChannelStats(ctx context.Context, id uint32) (ChannelStats, error) {
	var out ChannelStats
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/channels/%d/stats", id), nil, nil, &out)
	return out, err
}
