package uvk5

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

var fastFlash = FlashTiming{Detect: time.Second, PageReply: 300 * time.Millisecond, PageRetries: 3}

func bootPipe(t *testing.T, b *FakeBootloader) net.Conn {
	t.Helper()
	host, dev := net.Pipe()
	go func() { _ = b.Serve(dev) }()
	t.Cleanup(func() { _ = host.Close(); _ = dev.Close() })
	return host
}

func firmware(n int) []byte {
	fw := make([]byte, n)
	for i := range fw {
		fw[i] = byte(i * 7)
	}
	return fw
}

func TestFlash(t *testing.T) {
	b := NewFakeBootloader("7.02.02")
	b.FailPage = 2 // one page fails once and is sent again
	fw := firmware(5*flashPage + 100)
	var last FlashProgress
	bl, err := Flash(context.Background(), bootPipe(t, b), fw, fastFlash, func(p FlashProgress) { last = p })
	if err != nil {
		t.Fatal(err)
	}
	if bl.Version != "7.02.02" || string(bl.UID[:4]) != "0123" || last != (FlashProgress{Page: 6, Pages: 6, Bootloader: "7.02.02"}) {
		t.Fatalf("bootloader %+v, progress %+v", bl, last)
	}
	if img := b.Image(); !b.Done() || !bytes.Equal(img[:len(fw)], fw) || !bytes.Equal(img[len(fw):], make([]byte, 6*flashPage-len(fw))) {
		t.Fatal("image differs")
	}
}

func TestFlashRefusals(t *testing.T) {
	if _, err := Flash(context.Background(), bootPipe(t, NewFakeBootloader("7.00.06")), firmware(10), fastFlash, nil); !errors.Is(err, ErrBootloaderTooOld) {
		t.Errorf("old bootloader: %v", err)
	}
	quiet, other := net.Pipe()
	defer quiet.Close()
	defer other.Close()
	go func() { _, _ = discardAll(other) }()
	if _, err := Flash(context.Background(), quiet, firmware(10), FlashTiming{Detect: 200 * time.Millisecond, PageReply: time.Second}, nil); !errors.Is(err, ErrNoBootloader) {
		t.Errorf("no bootloader: %v", err)
	}
	if err := ValidFirmware(nil); err == nil {
		t.Error("empty firmware accepted")
	}
	if err := ValidFirmware(make([]byte, MaxFirmwareSize+1)); err == nil {
		t.Error("oversized firmware accepted")
	}
}

// A page that never succeeds stops the flash with a clear error.
func TestFlashGivesUp(t *testing.T) {
	b := NewFakeBootloader("7.02.02")
	host, dev := net.Pipe()
	defer host.Close()
	defer dev.Close()
	go func() { // announce, take the handshake, then never answer a page
		_ = b.Serve(&dropPages{dev})
	}()
	_, err := Flash(context.Background(), host, firmware(300), FlashTiming{Detect: time.Second, PageReply: 50 * time.Millisecond, PageRetries: 2}, nil)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("bootloader mode")) {
		t.Fatalf("err = %v", err)
	}
}

// dropPages swallows the bootloader's page acknowledgements.
type dropPages struct{ net.Conn }

func (d *dropPages) Write(p []byte) (int, error) {
	if r, err := NewReader(bytes.NewReader(p)).Next(); err == nil && r.Frame != nil && r.Frame.ID == msgProgFWAck {
		return len(p), nil
	}
	return d.Conn.Write(p)
}

func discardAll(c net.Conn) (int, error) {
	b := make([]byte, 256)
	for {
		if _, err := c.Read(b); err != nil {
			return 0, err
		}
	}
}

func TestVersionLess(t *testing.T) {
	for _, c := range []struct {
		a, b string
		less bool
	}{{"7.00.06", "7.00.07", true}, {"7.00.07", "7.00.07", false}, {"7.2", "7.00.07", false}, {"6.99.99", "7.00.07", true}, {"x", "7.00.07", true}} {
		if versionLess(c.a, c.b) != c.less {
			t.Errorf("%s < %s: want %v", c.a, c.b, c.less)
		}
	}
}
