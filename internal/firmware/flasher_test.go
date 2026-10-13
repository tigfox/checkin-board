package firmware

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"checkin-board/internal/uvk5"
)

type pipePort struct {
	net.Conn
	dtr *atomic.Bool
}

func (p pipePort) SetDTR(on bool) error { p.dtr.Store(on); return nil }

// bench: an AIOC on /dev/ttyACM0 (refused), and the radio, which appears
// in bootloader mode on /dev/ttyACM1 after appearAfter.
type bench struct {
	boot        *uvk5.FakeBootloader
	appearAfter time.Time
	lent        atomic.Int32
	given       atomic.Int32
	aiocOpened  atomic.Bool
	dtr         atomic.Bool
	mu          sync.Mutex
	conns       []net.Conn
}

func (b *bench) config(t *testing.T, fo *Folder) FlasherConfig {
	return FlasherConfig{
		Folder: fo,
		Lend: func(context.Context, string) (func(), error) {
			b.lent.Add(1)
			return func() { b.given.Add(1) }, nil
		},
		Ports: func() []string {
			if time.Now().Before(b.appearAfter) {
				return []string{"/dev/ttyACM0"}
			}
			return []string{"/dev/ttyACM0", "/dev/ttyACM1"}
		},
		Check: func(p string) error {
			if p == "/dev/ttyACM0" {
				return errors.New("an AIOC")
			}
			return nil
		},
		Open: func(p string) (Port, error) {
			if p == "/dev/ttyACM0" {
				b.aiocOpened.Store(true)
			}
			host, dev := net.Pipe()
			go func() { _ = b.boot.Serve(dev) }()
			b.mu.Lock()
			b.conns = append(b.conns, host, dev)
			b.mu.Unlock()
			return pipePort{host, &b.dtr}, nil
		},
		Timing: uvk5.FlashTiming{Detect: time.Second, PageReply: 300 * time.Millisecond, PageRetries: 3},
		Wait:   5 * time.Second,
	}
}

func (b *bench) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.conns {
		_ = c.Close()
	}
}

func waitState(t *testing.T, f *Flasher, want ...string) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := f.Status()
		for _, w := range want {
			if st.State == w {
				return st
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %+v, want %v", f.Status(), want)
	return Status{}
}

func TestFlasher(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	fw := bytes.Repeat([]byte{0xA5}, 3*256+10)
	if err := fo.Save("fw.bin", fw, false); err != nil {
		t.Fatal(err)
	}
	b := &bench{boot: uvk5.NewFakeBootloader("7.02.02"), appearAfter: time.Now().Add(700 * time.Millisecond)}
	defer b.close()
	f := NewFlasher(b.config(t, fo))
	if err := f.Start("fw.bin"); err != nil {
		t.Fatal(err)
	}
	if err := f.Start("fw.bin"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second start: %v", err)
	}
	if st := f.Status(); st.State != StateWaiting || b.lent.Load() != 1 {
		t.Fatalf("waiting: %+v, lent %d", st, b.lent.Load())
	}
	st := waitState(t, f, StateDone, StateFailed)
	if st.State != StateDone || st.Pages != 4 || st.Page != 4 || st.Bootloader != "7.02.02" || st.Port != "/dev/ttyACM1" {
		t.Fatalf("done = %+v", st)
	}
	if b.aiocOpened.Load() || !b.dtr.Load() || b.given.Load() != 1 || !bytes.Equal(b.boot.Image()[:len(fw)], fw) {
		t.Fatalf("aioc opened %v, dtr %v, given back %d", b.aiocOpened.Load(), b.dtr.Load(), b.given.Load())
	}
}

func TestFlasherCancelAndTimeout(t *testing.T) {
	fo := NewFolder(filepath.Join(t.TempDir(), "fw"))
	_ = fo.Save("fw.bin", []byte("x"), false)
	b := &bench{boot: uvk5.NewFakeBootloader("7.02.02"), appearAfter: time.Now().Add(time.Hour)}
	defer b.close()
	cfg := b.config(t, fo)
	f := NewFlasher(cfg)
	if err := f.Start("fw.bin"); err != nil {
		t.Fatal(err)
	}
	if err := f.Cancel(); err != nil {
		t.Fatal(err)
	}
	if st := waitState(t, f, StateFailed); st.Error != "cancelled" || b.given.Load() != 1 {
		t.Fatalf("cancelled = %+v, given %d", st, b.given.Load())
	}
	cfg.Wait = 300 * time.Millisecond
	f = NewFlasher(cfg)
	_ = f.Start("fw.bin")
	if st := waitState(t, f, StateFailed); st.Error == "" || b.given.Load() != 2 {
		t.Fatalf("timeout = %+v", st)
	}
	if err := f.Start("missing.bin"); err == nil {
		t.Fatal("missing file started")
	}
	if err := f.Cancel(); err == nil {
		t.Fatal("cancel with nothing running")
	}
}
