package radiocheck

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/hostmon"
	"checkin-board/internal/store"
)

// MissingCards names the sound cards the race's radio channel is set to
// use that aren't on this node now: the radio cable is unplugged, or it
// dropped off USB. In the field test of 2026-10-11 an AIOC dropped off a
// Pi Zero's USB after a transmit, and graywolf's API went on listing the
// device and its last level. ok is false when it can't tell (cards
// unknown, graywolf unreachable); with ok true, nil means every card it
// can identify is there (or there is no modem channel to check).
func MissingCards(ctx context.Context, gw Graywolf, cfg store.Settings, host hostmon.Snapshot) (missing []string, ok bool) {
	if !host.SoundCardsKnown {
		return nil, false
	}
	chans, err := gw.Channels(ctx)
	if err != nil {
		return nil, false
	}
	ch, _ := pickChannelFrom(chans, cfg)
	if ch == nil || !ch.Backing.Modem.Active {
		return nil, true
	}
	devs, err := gw.AudioDevices(ctx)
	if err != nil {
		return nil, false
	}
	return missingCards(*ch, devs, host), true
}

func missingCards(ch graywolf.Channel, devs []graywolf.AudioDevice, host hostmon.Snapshot) []string {
	if !host.SoundCardsKnown {
		return nil
	}
	var out []string
	for _, id := range []uint32{ch.InputDeviceID, ch.OutputDeviceID} {
		d := findDevice(devs, id)
		if d == nil || (d.SourceType != "" && d.SourceType != "soundcard") {
			continue
		}
		if name, present, known := cardPresent(d.DevicePath, host.SoundCards); known && !present && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	return out
}

// cardPlugins are the ALSA device names whose argument is a card
// ("hw:1,0", "plughw:CARD=AllInOneCable,DEV=0"). Others ("plug:default",
// "plug:'hw:1,0'", "pulse") name another device, not a card.
var cardPlugins = map[string]bool{
	"hw": true, "plughw": true, "sysdefault": true, "default": true, "front": true, "rear": true,
	"side": true, "center_lfe": true, "iec958": true, "hdmi": true, "dsnoop": true, "dmix": true,
}

// cardIDRe is an ALSA card id or index.
var cardIDRe = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// cardPresent reads the card out of an ALSA device name and looks it up.
// known is false whenever the name doesn't plainly name a card: a false
// "missing" would fail a working node, so doubt means unknown.
func cardPresent(path string, cards []hostmon.SoundCard) (name string, present, known bool) {
	plugin, args, ok := strings.Cut(path, ":")
	if !ok || args == "" || !(cardPlugins[plugin] || strings.HasPrefix(plugin, "surround")) {
		return "", false, false
	}
	parts := strings.Split(args, ",")
	card := ""
	for _, p := range parts {
		if v, ok := strings.CutPrefix(p, "CARD="); ok {
			card = v
		}
	}
	if card == "" && !strings.Contains(parts[0], "=") {
		card = parts[0] // positional: "hw:1,0"
	}
	if !cardIDRe.MatchString(card) {
		return "", false, false
	}
	if n, err := strconv.Atoi(card); err == nil {
		return fmt.Sprintf("card %d", n), slices.ContainsFunc(cards, func(c hostmon.SoundCard) bool { return c.Index == n }), true
	}
	return card, slices.ContainsFunc(cards, func(c hostmon.SoundCard) bool { return strings.EqualFold(c.ID, card) }), true
}
