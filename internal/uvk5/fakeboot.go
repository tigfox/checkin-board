package uvk5

import (
	"io"
	"sync"
	"time"
)

// FakeBootloader is a radio in bootloader mode, for tests: it announces
// itself every Every until the handshake, then writes the pages it gets
// into Image. FailPage makes the first try of that page fail.
type FakeBootloader struct {
	Version  string
	Every    time.Duration
	FailPage int // -1: none

	mu    sync.Mutex
	image []byte
	pages int
	done  bool
}

// NewFakeBootloader returns a bootloader with version v.
func NewFakeBootloader(v string) *FakeBootloader {
	return &FakeBootloader{Version: v, Every: 10 * time.Millisecond, FailPage: -1}
}

// Image returns what was written; Done whether every page was.
func (b *FakeBootloader) Image() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.image...)
}

func (b *FakeBootloader) Done() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done
}

// Serve runs the bootloader on rw until it fails.
func (b *FakeBootloader) Serve(rw io.ReadWriter) error {
	var wmu sync.Mutex
	write := func(id uint16, data []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		_, err := rw.Write(EncodeFrame(id, data, true))
		return err
	}
	stop := make(chan struct{})
	go func() { // announce until three handshakes arrive
		info := make([]byte, 32)
		copy(info, "0123456789abcdef")
		copy(info[16:], b.Version)
		t := time.NewTicker(b.Every)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if write(msgDevInfo, info) != nil {
					return
				}
			}
		}
	}()
	handshakes, failed := 0, false
	r := NewReader(rw)
	for {
		it, err := r.Next()
		if err != nil {
			if handshakes < 3 {
				close(stop)
			}
			return err
		}
		if it.Frame == nil {
			continue
		}
		switch it.Frame.ID {
		case msgBLVersion:
			if handshakes++; handshakes == 3 {
				close(stop)
			}
		case msgProgFW:
			d := it.Frame.Data
			page, pages := int(le16(d[4:])), int(le16(d[6:]))
			reply := make([]byte, 8)
			put16(reply[4:], uint16(page))
			if page == b.FailPage && !failed {
				failed = true
				put16(reply[6:], 1)
			} else {
				b.mu.Lock()
				if need := (page + 1) * flashPage; len(b.image) < need {
					b.image = append(b.image, make([]byte, need-len(b.image))...)
				}
				copy(b.image[page*flashPage:], d[12:12+flashPage])
				b.pages++
				b.done = page == pages-1
				b.mu.Unlock()
			}
			if err := write(msgProgFWAck, reply); err != nil {
				return err
			}
		}
	}
}
