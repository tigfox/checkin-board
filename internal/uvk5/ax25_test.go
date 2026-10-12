package uvk5

import (
	"reflect"
	"testing"
)

func TestAX25RoundTrip(t *testing.T) {
	p := Packet{Dest: "APZK5", Src: "KD2DCM-4", Path: []string{"WIDE1-1*", "WIDE2-1"}, Info: FormatMessage(Message{Addressee: "KD2DCM-1", Text: "RC1 H CTL 3 130000", ID: "07"})}
	raw, err := EncodeAX25(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeAX25(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, p) {
		t.Fatalf("got %+v\nwant %+v", got, p)
	}
	// Through an APRSRAW: line too.
	h, ok := ParseLine(FormatHeard(raw)).(Heard)
	if !ok || !reflect.DeepEqual(h.Packet, p) {
		t.Fatalf("line: %+v", h)
	}
}

func TestAX25Errors(t *testing.T) {
	good, _ := EncodeAX25(Packet{Dest: "APZK5", Src: "N0CALL", Info: []byte(">hi")})
	for name, b := range map[string][]byte{
		"empty":      nil,
		"short":      good[:10],
		"no control": good[:14],
		"not UI":     append(append([]byte{}, good[:14]...), 0x13, 0xF0),
	} {
		if _, err := DecodeAX25(b); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// One source address and no "last" bit anywhere: runs out.
	noLast := append([]byte{}, good[:14]...)
	noLast[13] &^= 1
	if _, err := DecodeAX25(noLast); err == nil {
		t.Error("no last-address bit: want an error")
	}
	if _, err := EncodeAX25(Packet{Dest: "TOOLONGCALL", Src: "N0CALL"}); err == nil {
		t.Error("long callsign: want an error")
	}
}

func TestParseMessage(t *testing.T) {
	for in, want := range map[string]Message{
		":KD2DCM-1 :RC1 R CTL 1 @1300 12/05{050": {Addressee: "KD2DCM-1", Text: "RC1 R CTL 1 @1300 12/05", ID: "050"},
		":KD2DCM-4 :ack07":                       {Addressee: "KD2DCM-4", ID: "07", Ack: true},
		":KD2DCM-4 :rej07":                       {Addressee: "KD2DCM-4", ID: "07", Rej: true},
		":KD2DCM-4 :hello":                       {Addressee: "KD2DCM-4", Text: "hello"},
		":KD2DCM-4 :hi{12}AB":                    {Addressee: "KD2DCM-4", Text: "hi", ID: "12"},
		":KD2DCM-4 :ackno, a word{3":             {Addressee: "KD2DCM-4", Text: "ackno, a word", ID: "3"},
	} {
		got, ok := ParseMessage([]byte(in))
		if !ok || got != want {
			t.Errorf("%q: got %+v (%v), want %+v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", ">status", ":SHORT:x", "!4000.00N/07500.00W-"} {
		if _, ok := ParseMessage([]byte(bad)); ok {
			t.Errorf("%q: want not a message", bad)
		}
	}
}

func TestFormatMessage(t *testing.T) {
	for m, want := range map[Message]string{
		{Addressee: "N0CALL-7", Text: "hi", ID: "05"}: ":N0CALL-7 :hi{05",
		{Addressee: "N0CALL", ID: "050", Ack: true}:   ":N0CALL   :ack050",
		{Addressee: "N0CALL", ID: "1", Rej: true}:     ":N0CALL   :rej1",
		{Addressee: "N0CALL", Text: "no id"}:          ":N0CALL   :no id",
	} {
		if got := string(FormatMessage(m)); got != want {
			t.Errorf("%+v: got %q want %q", m, got, want)
		}
		if back, _ := ParseMessage(FormatMessage(m)); back != m {
			t.Errorf("round trip %+v -> %+v", m, back)
		}
	}
}

func TestSplitCallsign(t *testing.T) {
	for in, want := range map[string]struct {
		call string
		ssid uint8
		ok   bool
	}{
		"KD2DCM-4": {"KD2DCM", 4, true}, "kd2dcm": {"KD2DCM", 0, true}, "N0CALL-15": {"N0CALL", 15, true},
		"N0CALL-16": {ok: false}, "TOOLONG1": {ok: false}, "": {ok: false}, "BAD!": {ok: false}, "X-a": {ok: false},
	} {
		call, ssid, err := SplitCallsign(in)
		if (err == nil) != want.ok || (want.ok && (call != want.call || ssid != want.ssid)) {
			t.Errorf("%q: got %q %d %v", in, call, ssid, err)
		}
	}
}

func TestParseLine(t *testing.T) {
	for in, want := range map[string]any{
		"SENT:KD2DCM-1{07}":              Sent{To: "KD2DCM-1", Line: "07"},
		"SENT:kd2dcm-1{07":               Sent{To: "KD2DCM-1", Line: "07"},
		"DROP:KD2DCM-1{07} busy":         Dropped{To: "KD2DCM-1", Line: "07", Reason: ReasonBusy},
		"DROP:ack KD2DCM-1{050} battery": Dropped{To: "KD2DCM-1", Line: "050", Ack: true, Reason: ReasonBattery},
		"DROP:KD2DCM-1{07} whatever":     Dropped{To: "KD2DCM-1", Line: "07", Reason: ReasonUnknown},
		"DROP:KD2DCM-1{07} mode":         Dropped{To: "KD2DCM-1", Line: "07", Reason: ReasonMode},
		"APRSRSSI:-87":                   HeardRSSI{DBm: -87},
		"APRSRSSI:x":                     OtherLine{"APRSRSSI:x"},
		"APRSRAW:ZZ":                     OtherLine{"APRSRAW:ZZ"},
		"APRSRAW:00":                     OtherLine{"APRSRAW:00"},
		"DIGI:NOTX W1ABC":                OtherLine{"DIGI:NOTX W1ABC"},
		"SENT:{07}":                      OtherLine{"SENT:{07}"},
	} {
		if got := ParseLine(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %#v want %#v", in, got, want)
		}
	}
}

func TestReasonNames(t *testing.T) {
	for r := ReasonFull; r <= ReasonStore; r++ {
		if ParseReason(r.String()) != r {
			t.Errorf("%d: %q doesn't round trip", r, r)
		}
	}
	if Reason(200).String() != "reason-200" || ReasonNone.String() != "ok" || ParseReason("nope") != ReasonUnknown {
		t.Error("reason names")
	}
	// The spec's pinned numbers (5.7).
	for r, n := range map[Reason]uint8{ReasonFull: 1, ReasonSession: 2, ReasonBusy: 5, ReasonBattery: 9, ReasonOff: 12, ReasonStore: 14} {
		if uint8(r) != n {
			t.Errorf("%s = %d, want %d", r, r, n)
		}
	}
}
