package uvk5

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
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
