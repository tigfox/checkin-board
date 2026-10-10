package gwfake

import (
	"context"
	"fmt"
	"net/http"

	"checkin-board/internal/graywolf"
)

// RadioSetup is what graywolf's read-only radio setup endpoints report.
type RadioSetup struct {
	Commit   string
	Devices  []graywolf.AudioDevice
	Levels   map[uint32]graywolf.DeviceLevel
	Channels []graywolf.Channel
	Stats    map[uint32]graywolf.ChannelStats
}

// DefaultRadioSetup is a working AIOC channel at 48 kHz, as on a faster
// Pi (a Pi Zero also needs 24 kHz and the fixed build).
func DefaultRadioSetup() RadioSetup {
	ch := graywolf.Channel{ID: 1, Name: "VHF APRS", Mode: "aprs", Enabled: true, InputDeviceID: 1, OutputDeviceID: 2,
		ModemType: "afsk", BitRate: 1200, PTT: graywolf.ChannelPTT{Method: "cm108", Configured: true}}
	ch.Backing.Summary, ch.Backing.Health, ch.Backing.Modem.Active, ch.Backing.TX.Capable = "modem", "live", true, true
	aioc := "plughw:CARD=AllInOneCable,DEV=0"
	return RadioSetup{
		Commit: "b589e686",
		Devices: []graywolf.AudioDevice{
			{ID: 1, Name: "All-In-One-Cable", Direction: "input", SourceType: "soundcard", DevicePath: aioc, SampleRate: 48000, Channels: 1},
			{ID: 2, Name: "All-In-One-Cable", Direction: "output", SourceType: "soundcard", DevicePath: aioc, SampleRate: 48000, Channels: 1},
		},
		Levels:   map[uint32]graywolf.DeviceLevel{1: {DeviceID: 1, PeakDBFS: -45}, 2: {DeviceID: 2, PeakDBFS: -60}},
		Channels: []graywolf.Channel{ch},
		Stats:    map[uint32]graywolf.ChannelStats{1: {Channel: 1, RxFrames: 3}},
	}
}

// EditRadio changes the radio setup under the station's lock (safe while
// a server is reading it).
func (s *Station) EditRadio(edit func(*RadioSetup)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	edit(&s.setup)
}

// AudioDevices implements graywolf.Client.AudioDevices.
func (s *Station) AudioDevices(ctx context.Context) ([]graywolf.AudioDevice, error) {
	if err := s.unavailable(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]graywolf.AudioDevice(nil), s.setup.Devices...), nil
}

// AudioLevels implements graywolf.Client.AudioLevels.
func (s *Station) AudioLevels(ctx context.Context) (map[uint32]graywolf.DeviceLevel, error) {
	if err := s.unavailable(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[uint32]graywolf.DeviceLevel, len(s.setup.Levels))
	for k, v := range s.setup.Levels {
		out[k] = v
	}
	return out, nil
}

// Channels implements graywolf.Client.Channels.
func (s *Station) Channels(ctx context.Context) ([]graywolf.Channel, error) {
	if err := s.unavailable(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]graywolf.Channel(nil), s.setup.Channels...), nil
}

// ChannelStats implements graywolf.Client.ChannelStats.
func (s *Station) ChannelStats(ctx context.Context, id uint32) (graywolf.ChannelStats, error) {
	if err := s.unavailable(); err != nil {
		return graywolf.ChannelStats{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.setup.Stats[id]
	if !ok {
		return graywolf.ChannelStats{}, &graywolf.APIError{Method: "GET", Path: fmt.Sprintf("/api/channels/%d/stats", id),
			StatusCode: http.StatusNotFound, Message: "channel not found"}
	}
	return st, nil
}
