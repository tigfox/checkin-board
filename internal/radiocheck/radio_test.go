package radiocheck

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/radiobackend"
	"checkin-board/internal/uvk5"
)

type stubRadio struct {
	state  radiobackend.State
	status uvk5.Status
	err    error
}

func (s *stubRadio) State() radiobackend.State { return s.state }
func (s *stubRadio) RadioStatus(context.Context) (uvk5.Status, error) {
	return s.status, s.err
}

func goodRadio() *stubRadio {
	return &stubRadio{
		state: radiobackend.State{Connected: true, Firmware: "f4hwn.aprs.2", Callsign: "KD2DCM-4", APRSOn: true,
			Caps: uvk5.Capabilities{Revision: 1, Features: 0x3ff, QueueDepth: 4, AckDepth: 4, MaxText: 67}},
		status: uvk5.Status{Monitor: true, WouldTransmit: true, InBand: true, BatteryMV: 7420, Decoded: 12,
			FreqHz: 144_390_000, Damaged: 1, Sent: 3, FreeSlots: 4},
	}
}

func TestRadioHealthy(t *testing.T) {
	r := NewChecker(nil).CheckRadio(ctx, goodRadio())
	if r.Status != OK || len(r.Items) != 8 {
		t.Fatalf("report = %+v", r)
	}
	if f := item(t, r, "frequency"); !strings.Contains(f.Detail, "144.3900 MHz") {
		t.Fatalf("frequency = %+v", f)
	}
	if b := item(t, r, "battery"); b.Detail != "7.42 V" {
		t.Fatalf("battery = %+v", b)
	}
}

func TestRadioNotConnected(t *testing.T) {
	r := NewChecker(nil).CheckRadio(ctx, &stubRadio{state: radiobackend.State{Error: "uvk5: no radio found"}})
	if r.Status != Fail || item(t, r, "radio").Status != Fail || item(t, r, "callsign").Status != Unknown {
		t.Fatalf("report = %+v", r)
	}
	old := NewChecker(nil).CheckRadio(ctx, &stubRadio{state: radiobackend.State{Error: uvk5.ErrFirmware.Error()}})
	if d := item(t, old, "radio").Detail; !strings.Contains(d, "reflash") {
		t.Fatalf("old firmware detail = %q", d)
	}
}

func TestRadioWontTransmit(t *testing.T) {
	for reason, want := range map[uvk5.Reason]string{
		uvk5.ReasonBattery: "battery", uvk5.ReasonBand: "frequency", uvk5.ReasonMode: "FM",
		uvk5.ReasonAPRSOff: "APRS", uvk5.ReasonNoCall: "callsign", uvk5.ReasonStore: "store",
	} {
		r := goodRadio()
		r.status.WouldTransmit, r.status.Inhibit = false, reason
		if reason == uvk5.ReasonBand {
			r.status.InBand = false
		}
		rep := NewChecker(nil).CheckRadio(ctx, r)
		if tx := item(t, rep, "transmit"); tx.Status != Fail || !strings.Contains(tx.Detail, want) {
			t.Errorf("%s: transmit = %+v", reason, tx)
		}
	}
	busy := goodRadio()
	busy.status.WouldTransmit, busy.status.Busy = false, true
	if tx := item(t, NewChecker(nil).CheckRadio(ctx, busy), "transmit"); tx.Status != Warn {
		t.Errorf("busy: %+v", tx)
	}
}

func TestRadioCallsignAndMonitor(t *testing.T) {
	r := goodRadio()
	r.state.Callsign = "N0CALL-7"
	r.status.Monitor = false
	rep := NewChecker(nil).CheckRadio(ctx, r)
	if item(t, rep, "callsign").Status != Fail || item(t, rep, "monitor").Status != Fail {
		t.Fatalf("report = %+v", rep)
	}
	r = goodRadio()
	r.state.APRSOn = false
	if c := item(t, NewChecker(nil).CheckRadio(ctx, r), "callsign"); c.Status != Fail || !strings.Contains(c.Detail, "APRS") {
		t.Fatalf("APRS off: %+v", c)
	}
}

func TestRadioHeardDelta(t *testing.T) {
	clk := time.Date(2026, 10, 12, 13, 0, 0, 0, time.UTC)
	c := NewChecker(func() time.Time { return clk })
	r := goodRadio()
	c.CheckRadio(ctx, r)
	clk = clk.Add(time.Minute)
	r.status.Decoded, r.status.Damaged = 13, 9 // 1 good, 8 damaged since
	if h := item(t, c.CheckRadio(ctx, r), "heard"); h.Status != Warn || !strings.Contains(h.Detail, "since the last check 1m0s") {
		t.Fatalf("heard = %+v", h)
	}
	quiet := goodRadio()
	quiet.status.Decoded = 0
	if h := item(t, NewChecker(nil).CheckRadio(ctx, quiet), "heard"); h.Status != Warn {
		t.Fatalf("nothing heard = %+v", h)
	}
}

func TestRadioQueueFullAndNoStatus(t *testing.T) {
	r := goodRadio()
	r.status.FreeSlots = 0
	if q := item(t, NewChecker(nil).CheckRadio(ctx, r), "queue"); q.Status != Warn {
		t.Fatalf("queue = %+v", q)
	}
	r = goodRadio()
	r.err = uvk5.ErrUnsupported
	rep := NewChecker(nil).CheckRadio(ctx, r)
	if b := item(t, rep, "battery"); b.Status != Unknown || !strings.Contains(b.Detail, "no status command") {
		t.Fatalf("no status: %+v", b)
	}
	r.err = errors.New("timeout")
	if b := item(t, NewChecker(nil).CheckRadio(ctx, r), "battery"); !strings.Contains(b.Detail, "timeout") {
		t.Fatalf("status error: %+v", b)
	}
}
