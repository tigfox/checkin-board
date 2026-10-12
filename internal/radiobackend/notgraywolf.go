package radiobackend

import (
	"context"
	"errors"
	"net/http"

	"checkin-board/internal/graywolf"
)

// ErrNotGraywolf answers graywolf-only calls (audio devices, channels,
// TX timing, digipeater) on a radio node: they don't exist there.
var ErrNotGraywolf = errors.New("not used on a radio node (UV-K5/UV-K1): there's no graywolf")

func notGraywolf(path string) error {
	return &graywolf.APIError{Method: "RADIO", Path: path, StatusCode: http.StatusNotImplemented, Message: ErrNotGraywolf.Error()}
}

// Version reports the radio's firmware in graywolf's shape.
func (b *Backend) Version(ctx context.Context) (graywolf.Version, error) {
	st := b.State()
	if !st.Connected {
		return graywolf.Version{}, &graywolf.APIError{Method: "RADIO", Path: "/api/version", StatusCode: http.StatusServiceUnavailable, Message: st.Error}
	}
	return graywolf.Version{Version: st.Firmware, Commit: "uvk5", Platform: "uvk5"}, nil
}

func (b *Backend) AudioDevices(context.Context) ([]graywolf.AudioDevice, error) {
	return nil, notGraywolf("/api/audio-devices")
}

func (b *Backend) AudioLevels(context.Context) (map[uint32]graywolf.DeviceLevel, error) {
	return nil, notGraywolf("/api/audio-devices/levels")
}

func (b *Backend) Channels(context.Context) ([]graywolf.Channel, error) {
	return nil, notGraywolf("/api/channels")
}

func (b *Backend) ChannelStats(context.Context, uint32) (graywolf.ChannelStats, error) {
	return graywolf.ChannelStats{}, notGraywolf("/api/channels/stats")
}

func (b *Backend) TxTimings(context.Context) ([]graywolf.TxTiming, error) {
	return nil, notGraywolf("/api/tx-timing")
}

func (b *Backend) SetTxTiming(context.Context, graywolf.TxTiming) (graywolf.TxTiming, error) {
	return graywolf.TxTiming{}, notGraywolf("/api/tx-timing")
}

func (b *Backend) Digipeater(context.Context) (graywolf.Digipeater, error) {
	return graywolf.Digipeater{}, notGraywolf("/api/digipeater")
}

func (b *Backend) SetDigipeater(context.Context, graywolf.Digipeater) (graywolf.Digipeater, error) {
	return graywolf.Digipeater{}, notGraywolf("/api/digipeater")
}

// SetMessagePreferences: the radio has none to set (retention is the
// node's own database here).
func (b *Backend) SetMessagePreferences(ctx context.Context, p graywolf.MessagePreferences) (graywolf.MessagePreferences, error) {
	return graywolf.MessagePreferences{}, notGraywolf("/api/messages/preferences")
}
