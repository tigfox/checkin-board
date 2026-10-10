// Package radiocheck answers "is the radio set up in graywolf, and is it
// working?" (feedback 2026-10-09, item 6). Every failure in the two-node
// field test was invisible from outside: a silent carrier, received audio
// dropped without an error. The checklist reads graywolf's API
// (read-only) and the node's own health (hostmon).
package radiocheck

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/hostmon"
	"checkin-board/internal/store"
)

// Status of one item, or of the whole report (its worst item).
type Status string

const (
	OK      Status = "ok"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Unknown Status = "unknown" // couldn't be checked
)

// Item is one line of the checklist.
type Item struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Report is the checklist; Status is its worst item (unknown items don't
// count against it).
type Report struct {
	Status Status `json:"status"`
	Items  []Item `json:"items"`
}

// Graywolf is the read-only graywolf API the check uses.
type Graywolf interface {
	Version(ctx context.Context) (graywolf.Version, error)
	AudioDevices(ctx context.Context) ([]graywolf.AudioDevice, error)
	AudioLevels(ctx context.Context) (map[uint32]graywolf.DeviceLevel, error)
	Channels(ctx context.Context) ([]graywolf.Channel, error)
	ChannelStats(ctx context.Context, id uint32) (graywolf.ChannelStats, error)
}

const (
	// piZeroMachine is the Pi Zero W / Pi 1 kernel machine name.
	piZeroMachine = "armv6l"
	// piZeroBuildMark is in the commit of graywolf builds with the Pi
	// Zero fixes (deploy/graywolf-armv6).
	piZeroBuildMark = "armv6buf"
	// piZeroMaxRate is the fastest audio rate a Pi Zero keeps up with.
	piZeroMaxRate = 24000
	// silentDBFS: an input at or below this has no audio at all.
	silentDBFS = -59.0
	// minBadFrames before a damaged-frame majority is worth a warning.
	minBadFrames = 3
)

// Checker runs the checklist and remembers each channel's counters, so
// decode quality is judged on what changed since the previous check.
// Safe for concurrent use.
type Checker struct {
	now  func() time.Time
	mu   sync.Mutex
	last map[uint32]statsAt
}

type statsAt struct {
	at    time.Time
	stats graywolf.ChannelStats
}

// NewChecker returns a checker; now is the clock (nil: time.Now).
func NewChecker(now func() time.Time) *Checker {
	if now == nil {
		now = time.Now
	}
	return &Checker{now: now, last: map[uint32]statsAt{}}
}

// Check runs the checklist once, with no memory of earlier checks.
func Check(ctx context.Context, gw Graywolf, cfg store.Settings, host hostmon.Snapshot) Report {
	return NewChecker(nil).Check(ctx, gw, cfg, host)
}

// Check runs the checklist. host is the node's latest hostmon snapshot.
func (c *Checker) Check(ctx context.Context, gw Graywolf, cfg store.Settings, host hostmon.Snapshot) Report {
	piZero := host.Machine == piZeroMachine
	items := []Item{checkBuild(ctx, gw, piZero)}

	ch, chItem := pickChannel(ctx, gw, cfg)
	items = append(items, chItem)
	if ch == nil {
		for _, k := range []struct{ key, label string }{
			{"audio", "Audio devices"}, {"ptt", "Push-to-talk"}, {"rate", "Audio sample rate"},
			{"level", "Receive audio level"}, {"heard", "Packets decoded"},
		} {
			items = append(items, Item{k.key, k.label, Unknown, "needs a working radio channel first"})
		}
	} else if !ch.Backing.Modem.Active {
		// A KISS TNC does its own audio: nothing of graywolf's to check.
		const na = "not used: the channel is a KISS TNC, which handles its own audio"
		items = append(items, Item{"audio", "Audio devices", OK, na}, checkPTT(*ch),
			Item{"rate", "Audio sample rate", OK, na}, Item{"level", "Receive audio level", OK, na}, c.checkHeard(ctx, gw, *ch))
	} else {
		devs, err := gw.AudioDevices(ctx)
		items = append(items, checkAudio(*ch, devs, err), checkPTT(*ch), checkRate(*ch, devs, err, piZero),
			checkLevel(ctx, gw, *ch), c.checkHeard(ctx, gw, *ch))
	}
	items = append(items, checkKeepUp(host))
	return Report{Status: worst(items), Items: items}
}

func checkBuild(ctx context.Context, gw Graywolf, piZero bool) Item {
	it := Item{Key: "build", Label: "graywolf"}
	v, err := gw.Version(ctx)
	switch {
	case err != nil:
		it.Status, it.Detail = Fail, "can't reach graywolf: "+err.Error()
	case piZero && !strings.Contains(v.Commit, piZeroBuildMark):
		it.Status = Fail
		it.Detail = fmt.Sprintf("graywolf %s (%s) lacks the Pi Zero fixes: it transmits a silent carrier and drops received audio. Install the build from deploy/graywolf-armv6 (station guide, Pi Zero nodes).", v.Version, v.Commit)
	default:
		it.Status, it.Detail = OK, fmt.Sprintf("graywolf %s (%s)", v.Version, v.Commit)
	}
	return it
}

// pickChannel finds the channel the app sends on: Messaging settings'
// graywolf channel, or (0) the first enabled channel that can transmit.
func pickChannel(ctx context.Context, gw Graywolf, cfg store.Settings) (*graywolf.Channel, Item) {
	it := Item{Key: "channel", Label: "Radio channel"}
	chans, err := gw.Channels(ctx)
	if err != nil {
		it.Status, it.Detail = Fail, "can't read graywolf's channels: "+err.Error()
		return nil, it
	}
	var ch *graywolf.Channel
	for i := range chans {
		c := &chans[i]
		// As in the engines, a setting of 0 or less means automatic.
		if (cfg.GWChannel > 0 && c.ID == uint32(cfg.GWChannel)) || (cfg.GWChannel <= 0 && c.Enabled && c.Backing.TX.Capable) {
			ch = c
			break
		}
	}
	switch {
	case ch == nil && cfg.GWChannel > 0:
		it.Status, it.Detail = Fail, fmt.Sprintf("graywolf channel %d (Messaging settings) doesn't exist", cfg.GWChannel)
		return nil, it
	case ch == nil:
		it.Status, it.Detail = Fail, "no radio channel is set up in graywolf (Channels)"
		return nil, it
	case !ch.Enabled:
		it.Status, it.Detail = Fail, fmt.Sprintf("channel %d %q is disabled in graywolf", ch.ID, ch.Name)
		return nil, it
	case ch.Backing.Health != "live":
		// Still check its devices and PTT: they may be why it's down.
		it.Status, it.Detail = Fail, fmt.Sprintf("channel %d %q: graywolf's modem is %s, not live", ch.ID, ch.Name, ch.Backing.Health)
		return ch, it
	default:
		it.Status, it.Detail = OK, fmt.Sprintf("channel %d %q, modem live", ch.ID, ch.Name)
		return ch, it
	}
}

func findDevice(devs []graywolf.AudioDevice, id uint32) *graywolf.AudioDevice {
	for i := range devs {
		if devs[i].ID == id {
			return &devs[i]
		}
	}
	return nil
}

func checkAudio(ch graywolf.Channel, devs []graywolf.AudioDevice, err error) Item {
	it := Item{Key: "audio", Label: "Audio devices"}
	if err != nil {
		it.Status, it.Detail = Unknown, "can't read graywolf's audio devices: "+err.Error()
		return it
	}
	in, out := findDevice(devs, ch.InputDeviceID), findDevice(devs, ch.OutputDeviceID)
	var missing []string
	if in == nil {
		missing = append(missing, "input (receive)")
	}
	if out == nil {
		missing = append(missing, "output (transmit)")
	}
	if len(missing) > 0 {
		it.Status, it.Detail = Fail, "the channel's "+strings.Join(missing, " and ")+" audio device isn't set up in graywolf (Audio Devices)"
		return it
	}
	it.Status, it.Detail = OK, fmt.Sprintf("input %s, output %s", in.DevicePath, out.DevicePath)
	return it
}

func checkPTT(ch graywolf.Channel) Item {
	it := Item{Key: "ptt", Label: "Push-to-talk"}
	switch {
	case !ch.PTT.Configured:
		it.Status, it.Detail = Fail, "no push-to-talk configured: graywolf can't key the radio to transmit"
	case !ch.Backing.TX.Capable:
		it.Status, it.Detail = Fail, "the channel can't transmit"
	default:
		it.Status, it.Detail = OK, ch.PTT.Method
	}
	return it
}

func checkRate(ch graywolf.Channel, devs []graywolf.AudioDevice, err error, piZero bool) Item {
	it := Item{Key: "rate", Label: "Audio sample rate"}
	if err != nil {
		it.Status, it.Detail = Unknown, "can't read graywolf's audio devices"
		return it
	}
	var rates []string
	tooFast := false
	for _, id := range []uint32{ch.InputDeviceID, ch.OutputDeviceID} {
		if d := findDevice(devs, id); d != nil {
			rates = append(rates, fmt.Sprintf("%s %d Hz", d.Direction, d.SampleRate))
			tooFast = tooFast || d.SampleRate > piZeroMaxRate
		}
	}
	switch {
	case len(rates) == 0:
		it.Status, it.Detail = Unknown, "the channel's audio devices aren't set up"
	case piZero && tooFast:
		it.Status = Fail
		it.Detail = strings.Join(rates, ", ") + ": a Pi Zero can't keep up; set the AIOC input and output to 24 kHz (station guide, Pi Zero nodes)"
	default:
		it.Status, it.Detail = OK, strings.Join(rates, ", ")
	}
	return it
}

func checkLevel(ctx context.Context, gw Graywolf, ch graywolf.Channel) Item {
	it := Item{Key: "level", Label: "Receive audio level"}
	levels, err := gw.AudioLevels(ctx)
	lv, ok := levels[ch.InputDeviceID]
	switch {
	case err != nil:
		it.Status, it.Detail = Unknown, "can't read audio levels: "+err.Error()
	case !ok:
		it.Status, it.Detail = Unknown, "no level reported for the input device"
	case lv.Clipping:
		it.Status, it.Detail = Warn, fmt.Sprintf("input clipping (peak %.0f dBFS): turn the radio volume or input gain down", lv.PeakDBFS)
	case lv.PeakDBFS <= silentDBFS:
		it.Status, it.Detail = Warn, fmt.Sprintf("no audio on the input (peak %.0f dBFS): is the receiver connected and unmuted?", lv.PeakDBFS)
	default:
		it.Status, it.Detail = OK, fmt.Sprintf("peak %.0f dBFS", lv.PeakDBFS)
	}
	return it
}

// checkHeard judges decode quality on the change since the previous
// check (counters are since graywolf started), so hours of good traffic
// can't hide a receive path that just broke.
func (c *Checker) checkHeard(ctx context.Context, gw Graywolf, ch graywolf.Channel) Item {
	it := Item{Key: "heard", Label: "Packets decoded"}
	st, err := gw.ChannelStats(ctx, ch.ID)
	if err != nil {
		it.Status, it.Detail = Unknown, "can't read channel stats: "+err.Error()
		return it
	}
	now := c.now()
	c.mu.Lock()
	prev, seen := c.last[ch.ID]
	c.last[ch.ID] = statsAt{now, st}
	c.mu.Unlock()
	good, bad, scope := st.RxFrames, st.RxBadFCS, "since graywolf started"
	if seen && st.RxFrames >= prev.stats.RxFrames && st.RxBadFCS >= prev.stats.RxBadFCS {
		good, bad = st.RxFrames-prev.stats.RxFrames, st.RxBadFCS-prev.stats.RxBadFCS
		scope = fmt.Sprintf("since the last check %s ago", now.Sub(prev.at).Round(time.Second))
	}
	switch {
	case bad >= minBadFrames && bad > good:
		it.Status = Warn
		it.Detail = fmt.Sprintf("%d good and %d damaged frames %s: received audio is being lost or distorted", good, bad, scope)
	case st.RxFrames == 0:
		it.Status, it.Detail = Warn, "nothing decoded since graywolf started: no traffic heard yet, or receive isn't working"
	default:
		it.Status = OK
		it.Detail = fmt.Sprintf("%d good, %d damaged %s (%d decoded, %d sent since graywolf started)", good, bad, scope, st.RxFrames, st.TxFrames)
	}
	return it
}

func checkKeepUp(host hostmon.Snapshot) Item {
	it := Item{Key: "keepup", Label: "Modem keeping up"}
	switch {
	case host.ModemKeepingUp == nil:
		it.Status, it.Detail = Unknown, "not measured yet (needs graywolf's modem running for a minute)"
	case !*host.ModemKeepingUp:
		it.Status = Fail
		it.Detail = "graywolf's modem is falling behind: received audio is being dropped. On a Pi Zero, set the audio to 24 kHz and use the fixed graywolf build."
	default:
		it.Status, it.Detail = OK, fmt.Sprintf("waiting for audio %.0f times a second", host.ModemWaitsPerSec)
	}
	return it
}

func worst(items []Item) Status {
	out := OK
	for _, it := range items {
		switch {
		case it.Status == Fail:
			return Fail
		case it.Status == Warn:
			out = Warn
		}
	}
	return out
}
