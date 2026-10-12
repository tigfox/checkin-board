package uvk5

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.Join(strings.Fields(s), ""))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The spec's worked examples (section 3), session ts 0x11223344.
func TestGoldenHandshake(t *testing.T) {
	data := make([]byte, 4)
	put32(data, 0x11223344)
	want := unhex(t, "ab cd 08 00 02 69 10 e6 6a a2 2f 51 f2 22 dc ba")
	if got := EncodeFrame(cmdHandshake, data, false); !bytes.Equal(got, want) {
		t.Fatalf("handshake\n got %x\nwant %x", got, want)
	}
}

func TestGoldenMessage(t *testing.T) {
	// The example uses the older padded layout: ts, a 10-byte
	// destination, a 30-byte text field.
	data := make([]byte, 44)
	put32(data, 0x11223344)
	copy(data[4:], "N0CALL-7")
	copy(data[14:], "hello from the ridge")
	want := unhex(t, `ab cd 30 00 16 6b 38 e6 6a a2 2f 51 6f 05 96 01 5f 4f c4 b7 16 6c 7c 83 42 fd 62 60 47 47 ba 2d 33 77 81 e5 36 1e 7d 82
		49 f4 0d 40 21 35 d5 40 13 03 e9 80 66 0b dc ba`)
	got := EncodeFrame(cmdSendMessage, data, false)
	if !bytes.Equal(got, want) {
		t.Fatalf("message\n got %x\nwant %x", got, want)
	}
	// And it reads back, CRC valid.
	it, err := NewReader(bytes.NewReader(got)).Next()
	if err != nil || it.Frame == nil || it.Frame.ID != cmdSendMessage || !it.Frame.CRCOK || !bytes.Equal(it.Frame.Data, data) {
		t.Fatalf("read back: %+v %v", it, err)
	}
}

func TestReaderMixesFramesAndLines(t *testing.T) {
	var stream bytes.Buffer
	stream.WriteString("APRS:W1ABC>hello\r\n")
	stream.Write(EncodeFrame(cmdSendMessage+1, []byte{1, 7, 3, 0}, true))
	stream.WriteString("\r\nSENT:W1ABC{07}\r\n")
	stream.Write([]byte{0xAB, 0xCD, 0xFF, 0xFF}) // a bad size: skipped, reading goes on
	stream.WriteString("DROP:W1ABC{08} busy\r\n")

	r := NewReader(&stream)
	var got []string
	for {
		it, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		switch {
		case errors.Is(err, ErrFrame):
			got = append(got, "bad")
		case err != nil:
			t.Fatal(err)
		case it.Frame != nil:
			if it.Frame.CRCOK {
				t.Error("a reply's FF FF marker read as a valid CRC")
			}
			got = append(got, "frame")
		default:
			got = append(got, it.Line)
		}
	}
	want := "APRS:W1ABC>hello|frame|SENT:W1ABC{07}|bad|DROP:W1ABC{08} busy"
	if strings.Join(got, "|") != want {
		t.Fatalf("got %q\nwant %q", strings.Join(got, "|"), want)
	}
}

func TestReaderRejectsBadTrailer(t *testing.T) {
	f := EncodeFrame(cmdMonitor+1, []byte{1, 0, 0, 0}, true)
	f[len(f)-1] = 0
	if _, err := NewReader(bytes.NewReader(f)).Next(); !errors.Is(err, ErrFrame) {
		t.Fatalf("err = %v", err)
	}
}

func FuzzReader(f *testing.F) {
	f.Add(EncodeFrame(cmdSendMessage+1, []byte{1, 7, 3, 0}, true))
	f.Add([]byte("APRSRAW:82A0\r\nSENT:X{01}\r\n"))
	f.Add([]byte{0xAB, 0xCD, 4, 0, 1, 2, 3, 4, 5, 6, 0xDC, 0xBA})
	f.Fuzz(func(t *testing.T, b []byte) {
		r := NewReader(bytes.NewReader(b))
		for range 1000 {
			it, err := r.Next()
			if err != nil && !errors.Is(err, ErrFrame) {
				return
			}
			if it.Line != "" {
				_ = ParseLine(it.Line)
			}
		}
	})
}

// Spec 5.8's worked example: status command and reply.
func TestGoldenStatus(t *testing.T) {
	if got, want := EncodeFrame(cmdStatus, nil, false), unhex(t, "ab cd 04 00 02 6b 14 e6 e8 c5 dc ba"); !bytes.Equal(got, want) {
		t.Fatalf("command\n got %x\nwant %x", got, want)
	}
	wire := unhex(t, "ab cd 18 00 03 6b 00 e6 2f 9b 0d 40 dd 29 d7 41 4b 51 35 80 11 6c 18 e6 2d 90 0d 40 de ca dc ba")
	it, err := NewReader(bytes.NewReader(wire)).Next()
	if err != nil || it.Frame == nil || it.Frame.ID != cmdStatus+1 {
		t.Fatalf("reply: %+v %v", it, err)
	}
	st, err := parseStatus(it.Frame.Data)
	if err != nil {
		t.Fatal(err)
	}
	want := Status{WouldTransmit: true, InBand: true, BatteryMV: 7420, Decoded: 258, FreqHz: 144_390_000,
		Damaged: 7, Sent: 12, FreeSlots: 3, AcksWaiting: 1}
	if st != want {
		t.Fatalf("status\n got %+v\nwant %+v", st, want)
	}
	if got := EncodeFrame(cmdStatus+1, st.encode(), true); !bytes.Equal(got, wire) {
		t.Fatalf("re-encoded\n got %x\nwant %x", got, wire)
	}
}

// Spec 5.9's worked example: set callsign W1ABC-7, then both replies.
func TestGoldenSetCallsign(t *testing.T) {
	want := unhex(t, "ab cd 11 00 00 6b 19 e6 6a a2 2f 51 76 04 94 02 50 03 e9 80 11 be 57 dc ba")
	if got := EncodeFrame(cmdSetCallsign, setCallsignData(0x11223344, "W1ABC", 7), false); !bytes.Equal(got, want) {
		t.Fatalf("command\n got %x\nwant %x", got, want)
	}
	for wire, ok := range map[string]bool{
		"ab cd 08 00 01 6b 10 e6 2f 91 0d 40 de ca dc ba": true,
		"ab cd 08 00 01 6b 10 e6 2e 91 0d 4d de ca dc ba": false,
	} {
		it, err := NewReader(bytes.NewReader(unhex(t, wire))).Next()
		if err != nil || it.Frame.ID != cmdSetCallsign+1 || (it.Frame.Data[0] == 1) != ok {
			t.Fatalf("%s: %+v %v", wire, it, err)
		}
		if !ok && Reason(it.Frame.Data[3]) != ReasonInvalid {
			t.Fatalf("refusal reason %d", it.Frame.Data[3])
		}
	}
}

// Spec 5.2's worked record: W1ABC-7, APRS on, path direct, 6 s gap.
func TestGoldenSettingsRecord(t *testing.T) {
	rec := unhex(t, `41 50 52 31 01 01 8a 07 58 02 42 fe 57 31 41 42
		43 00 00 00 4e 30 43 41 4c 4c 2d 37 00 00 31 33
		30 37 31 32 38 31 30 35 39 39 34 30 35 00 52 69
		64 67 65 20 64 69 67 69 00 00 00 00 00 00 00 00
		00 00 00 00 00 00 00 00 00 00 00 00 00 00 00 00
		00 00 00 00 00 00 00 00 00 00 7a 1f 02 0c 00 00`)
	st, err := parseSettings(append([]byte{1, 0, 0, 0}, rec...))
	if err != nil {
		t.Fatal(err)
	}
	if st.Callsign != "W1ABC-7" || !st.APRSOn || st.Path != "" || st.Gap != 6*time.Second || !st.Configured() {
		t.Fatalf("settings = %+v", st)
	}
	rec[92], rec[93] = 0, 0
	st, _ = parseSettings(append([]byte{1, 0, 0, 0}, rec...))
	if st.Path != "WIDE1-1,WIDE2-1" || st.Gap != 5*time.Second {
		t.Fatalf("defaults = %+v", st)
	}
}

// A capability refusal is revision 0 (and so too old for us).
func TestCapabilitiesRefused(t *testing.T) {
	c, err := parseCapabilities(make([]byte, 12))
	if err != nil || c.Has(Required) || c.Revision != 0 {
		t.Fatalf("refused = %+v %v", c, err)
	}
	full := Capabilities{Revision: 1, Features: 0x3ff, QueueDepth: 4, AckDepth: 4, MaxText: 67}
	if back, _ := parseCapabilities(full.encode()); back != full || !back.Has(Required|FeatStatus|FeatSetCall|FeatRSSI|FeatCounters) {
		t.Fatalf("round trip = %+v", back)
	}
}
