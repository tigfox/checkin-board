package radiocheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"checkin-board/internal/radiobackend"
	"checkin-board/internal/uvk5"
)

// Radio is a radio node's link (radiobackend.Backend), for CheckRadio.
type Radio interface {
	State() radiobackend.State
	RadioStatus(ctx context.Context) (uvk5.Status, error)
}

type radioCounters struct {
	at               time.Time
	decoded, damaged uint16
}

// inhibitAdvice says what to do about each reason the radio won't
// transmit (spec 5.7).
var inhibitAdvice = map[uvk5.Reason]string{
	uvk5.ReasonNoCall:  "the radio has no callsign: set it on the Station page or the radio's menu",
	uvk5.ReasonAPRSOff: "APRS is switched off on the radio: switch it on",
	uvk5.ReasonBand:    "the transmit frequency is outside 144-148 / 420-450 MHz: set the race frequency on the radio",
	uvk5.ReasonBattery: "the battery is too low: charge or swap it",
	uvk5.ReasonMode:    "the transmit VFO isn't FM: set it to FM",
	uvk5.ReasonBusy:    "the channel or the radio is busy (scanning, a menu open?)",
}

// CheckRadio runs the checklist for a node that uses a UV-K5/UV-K1
// radio instead of graywolf (phase 17d).
func (c *Checker) CheckRadio(ctx context.Context, r Radio) Report {
	st := r.State()
	link := Item{Key: "radio", Label: "Radio (UV-K5/UV-K1)"}
	if !st.Connected {
		link.Status = Fail
		link.Detail = "not connected: " + st.Error + ". Is it plugged in by USB-C, switched on, and running the current APRS firmware?"
		if strings.Contains(st.Error, "firmware too old") {
			link.Detail = st.Error
		}
		items := []Item{link}
		for _, k := range []struct{ key, label string }{
			{"callsign", "Callsign"}, {"transmit", "Can transmit"}, {"frequency", "Frequency"}, {"battery", "Battery"},
			{"monitor", "Receiving"}, {"heard", "Packets decoded"}, {"queue", "Message queue"},
		} {
			items = append(items, Item{k.key, k.label, Unknown, "needs the radio connected first"})
		}
		return Report{Status: worst(items), Items: items}
	}
	link.Status = OK
	link.Detail = fmt.Sprintf("firmware %s, protocol revision %d", st.Firmware, st.Caps.Revision)
	items := []Item{link, radioCallsign(st)}

	status, err := r.RadioStatus(ctx)
	if err != nil {
		detail := "can't read the radio's status: " + err.Error()
		if errors.Is(err, uvk5.ErrUnsupported) {
			detail = "this firmware has no status command"
		}
		for _, k := range []struct{ key, label string }{
			{"transmit", "Can transmit"}, {"frequency", "Frequency"}, {"battery", "Battery"},
			{"monitor", "Receiving"}, {"heard", "Packets decoded"}, {"queue", "Message queue"},
		} {
			items = append(items, Item{k.key, k.label, Unknown, detail})
		}
		return Report{Status: worst(items), Items: items}
	}
	items = append(items, radioTransmit(status), radioFrequency(status), radioBattery(status), radioMonitor(status),
		c.radioHeard(status), radioQueue(status, st))
	return Report{Status: worst(items), Items: items}
}

func radioCallsign(st radiobackend.State) Item {
	it := Item{Key: "callsign", Label: "Callsign"}
	call, _, _ := strings.Cut(st.Callsign, "-")
	switch {
	case st.Callsign == "" || strings.EqualFold(call, "N0CALL"):
		it.Status, it.Detail = Fail, "the radio has the placeholder callsign: set this station's callsign-SSID (Station page or the radio's menu)"
	case !st.APRSOn:
		it.Status, it.Detail = Fail, st.Callsign+", but APRS is switched off on the radio"
	default:
		it.Status, it.Detail = OK, st.Callsign
	}
	return it
}

func radioTransmit(s uvk5.Status) Item {
	it := Item{Key: "transmit", Label: "Can transmit"}
	switch {
	case s.WouldTransmit:
		it.Status, it.Detail = OK, "yes"
	case s.Inhibit == uvk5.ReasonBusy || (s.Inhibit == uvk5.ReasonNone && s.Busy):
		it.Status, it.Detail = Warn, "not right now: "+inhibitAdvice[uvk5.ReasonBusy]
	default:
		advice, ok := inhibitAdvice[s.Inhibit]
		if !ok {
			advice = "the radio says: " + s.Inhibit.String()
		}
		it.Status, it.Detail = Fail, "no: "+advice
	}
	return it
}

func radioFrequency(s uvk5.Status) Item {
	it := Item{Key: "frequency", Label: "Frequency"}
	mhz := fmt.Sprintf("%.4f MHz", float64(s.FreqHz)/1e6)
	if s.InBand {
		it.Status, it.Detail = OK, mhz+" (receive); transmit frequency in band"
	} else {
		it.Status, it.Detail = Fail, mhz+" (receive); the transmit frequency is outside the allowed bands"
	}
	return it
}

func radioBattery(s uvk5.Status) Item {
	it := Item{Key: "battery", Label: "Battery"}
	v := fmt.Sprintf("%.2f V", float64(s.BatteryMV)/1000)
	if s.Inhibit == uvk5.ReasonBattery {
		it.Status, it.Detail = Fail, v+": too low to transmit"
	} else {
		it.Status, it.Detail = OK, v
	}
	return it
}

func radioMonitor(s uvk5.Status) Item {
	it := Item{Key: "monitor", Label: "Receiving"}
	if s.Monitor {
		it.Status, it.Detail = OK, "the radio reports what it hears to this node"
	} else {
		it.Status, it.Detail = Fail, "monitoring is off: the node can't hear acks or messages (reconnect the radio)"
	}
	return it
}

// radioHeard judges decoding on the counters' change since the previous
// check (they're 16-bit and wrap).
func (c *Checker) radioHeard(s uvk5.Status) Item {
	it := Item{Key: "heard", Label: "Packets decoded"}
	now := c.now()
	c.mu.Lock()
	prev := c.lastRadio
	c.lastRadio = &radioCounters{at: now, decoded: s.Decoded, damaged: s.Damaged}
	c.mu.Unlock()
	good, bad, scope := int(s.Decoded), int(s.Damaged), "since APRS was switched on"
	if prev != nil {
		good, bad = int(s.Decoded-prev.decoded), int(s.Damaged-prev.damaged)
		scope = fmt.Sprintf("since the last check %s ago", now.Sub(prev.at).Round(time.Second))
	}
	switch {
	case bad >= minBadFrames && bad > good:
		it.Status = Warn
		it.Detail = fmt.Sprintf("%d good and about %d damaged %s: weak signal or a bad antenna", good, bad, scope)
	case s.Decoded == 0:
		it.Status, it.Detail = Warn, "nothing decoded yet: no traffic heard, or receive isn't working"
	default:
		it.Status = OK
		it.Detail = fmt.Sprintf("%d good, about %d damaged %s (%d sent since power-up)", good, bad, scope, s.Sent)
	}
	return it
}

func radioQueue(s uvk5.Status, st radiobackend.State) Item {
	it := Item{Key: "queue", Label: "Message queue"}
	depth := st.Caps.QueueDepth
	it.Detail = fmt.Sprintf("%d of %d places free, %d acks waiting", s.FreeSlots, depth, s.AcksWaiting)
	it.Status = OK
	if s.FreeSlots == 0 {
		it.Status = Warn
		it.Detail += ": the radio is busy sending a backlog"
	}
	return it
}
