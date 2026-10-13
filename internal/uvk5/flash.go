package uvk5

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// Firmware flashing through the radio's bootloader, ported from UV
// Studio's flash.js (Apache-2.0, Armel FAUVEAU; see
// internal/web/uvstudio/NOTICE). The radio is put in bootloader mode by
// hand (off, hold PTT, on); it then announces itself many times a second.
// The host answers with the bootloader's version and sends the firmware
// in 256-byte pages, each acknowledged.

const (
	msgDevInfo   uint16 = 0x0518 // bootloader -> host, repeated: uid, version
	msgBLVersion uint16 = 0x0530 // host -> bootloader: the handshake
	msgProgFW    uint16 = 0x0519 // host -> bootloader: one page
	msgProgFWAck uint16 = 0x051A // bootloader -> host: page result

	flashPage = 256
	// MaxFirmwareSize bounds a firmware image (the radios have 128 KiB of
	// program flash; this leaves room for larger parts).
	MaxFirmwareSize = 512 << 10
	// MinBootloader is the oldest bootloader UV Studio flashes.
	MinBootloader = "7.00.07"
)

// FlashTiming bounds each step; DefaultFlashTiming is UV Studio's.
type FlashTiming struct {
	// Detect: how long to wait for the bootloader's announcements.
	Detect time.Duration
	// PageReply: how long to wait for one page's acknowledgement.
	PageReply time.Duration
	// PageRetries: how many times one page is tried again.
	PageRetries int
}

var DefaultFlashTiming = FlashTiming{Detect: 5 * time.Second, PageReply: 3 * time.Second, PageRetries: 3}

// Bootloader is what the radio said about itself in bootloader mode.
type Bootloader struct {
	UID     [16]byte
	Version string
}

// FlashProgress reports the flash: after the handshake (Page 0, the
// point of no return), then after each page.
type FlashProgress struct {
	Page, Pages int
	Bootloader  string
}

var (
	// ErrNoBootloader: no announcement arrived; the radio isn't in
	// bootloader mode (or isn't on this port).
	ErrNoBootloader = errors.New("uvk5: no bootloader answered: switch the radio off, hold PTT and switch it on")
	// ErrBootloaderTooOld: the bootloader is older than MinBootloader.
	ErrBootloaderTooOld = errors.New("uvk5: bootloader too old for this firmware")
)

// ValidFirmware checks an image is something the bootloader can take.
func ValidFirmware(fw []byte) error {
	switch {
	case len(fw) == 0:
		return errors.New("firmware file is empty")
	case len(fw) > MaxFirmwareSize:
		return fmt.Errorf("firmware file is %d bytes; at most %d", len(fw), MaxFirmwareSize)
	}
	return nil
}

// Flash writes fw through the bootloader on rw (which it doesn't close).
// progress, if set, is called after each page.
func Flash(ctx context.Context, rw io.ReadWriter, fw []byte, t FlashTiming, progress func(FlashProgress)) (Bootloader, error) {
	if err := ValidFirmware(fw); err != nil {
		return Bootloader{}, err
	}
	frames := make(chan Frame, 64)
	readErr := make(chan error, 1)
	go func() {
		r := NewReader(rw)
		for {
			it, err := r.Next()
			if errors.Is(err, ErrFrame) {
				continue
			}
			if err != nil {
				readErr <- err
				return
			}
			if it.Frame != nil {
				for { // announcements pile up while we write: drop the oldest
					select {
					case frames <- *it.Frame:
					default:
						select {
						case <-frames:
						default:
						}
						continue
					}
					break
				}
			}
		}
	}()
	f := flasher{rw: rw, frames: frames, readErr: readErr, t: t}
	bl, err := f.detect(ctx)
	if err != nil {
		return bl, err
	}
	if versionLess(bl.Version, MinBootloader) {
		return bl, fmt.Errorf("%w: it is %s, %s or later is needed", ErrBootloaderTooOld, bl.Version, MinBootloader)
	}
	if err := f.handshake(ctx, bl.Version); err != nil {
		return bl, err
	}
	pages := (len(fw) + flashPage - 1) / flashPage
	if progress != nil {
		progress(FlashProgress{Page: 0, Pages: pages, Bootloader: bl.Version})
	}
	return bl, f.program(ctx, fw, func(p FlashProgress) {
		if progress != nil {
			p.Bootloader = bl.Version
			progress(p)
		}
	})
}

type flasher struct {
	rw      io.ReadWriter
	frames  chan Frame
	readErr chan error
	t       FlashTiming
}

// next waits for the next frame until deadline.
func (f *flasher) next(ctx context.Context, deadline time.Time) (Frame, error) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case fr := <-f.frames:
		return fr, nil
	case err := <-f.readErr:
		f.readErr <- err // later calls see it too
		return Frame{}, fmt.Errorf("uvk5: radio port: %w", err)
	case <-timer.C:
		return Frame{}, errTimeout
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	}
}

var errTimeout = errors.New("timeout")

// detect waits for 5 announcements in a row at a plausible rate (as UV
// Studio does: 5 ms to 1 s apart), so a stray frame can't start a flash.
// The minimum gap also skips announcements that queued up unread.
func (f *flasher) detect(ctx context.Context) (Bootloader, error) {
	deadline := time.Now().Add(f.t.Detect)
	var last time.Time
	run := 0
	for {
		fr, err := f.next(ctx, deadline)
		if errors.Is(err, errTimeout) {
			return Bootloader{}, ErrNoBootloader
		}
		if err != nil {
			return Bootloader{}, err
		}
		if fr.ID != msgDevInfo {
			continue
		}
		now := time.Now()
		if dt := now.Sub(last); !last.IsZero() && dt >= 5*time.Millisecond && dt <= time.Second {
			run++
		} else {
			run = 1
		}
		last = now
		if run >= 5 {
			return parseDevInfo(fr.Data), nil
		}
	}
}

func parseDevInfo(d []byte) Bootloader {
	var bl Bootloader
	copy(bl.UID[:], d)
	if len(d) > 16 {
		v := d[16:min(len(d), 32)]
		if i := strings.IndexByte(string(v), 0); i >= 0 {
			v = v[:i]
		}
		bl.Version = string(v)
	}
	return bl
}

// handshake answers three announcements with the bootloader's version
// (its first 4 characters), paced as UV Studio does (50 ms before and
// after each), then lets the announcements stop.
func (f *flasher) handshake(ctx context.Context, version string) error {
	ver := make([]byte, 4)
	copy(ver, version)
	f.drain() // answer fresh announcements, not ones queued meanwhile
	deadline := time.Now().Add(f.t.Detect)
	for sent := 0; sent < 3; {
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return err
		}
		fr, err := f.next(ctx, deadline)
		if errors.Is(err, errTimeout) {
			return errors.New("uvk5: the bootloader stopped answering during the handshake")
		}
		if err != nil {
			return err
		}
		if fr.ID != msgDevInfo {
			continue
		}
		if _, err := f.rw.Write(EncodeFrame(msgBLVersion, ver, false)); err != nil {
			return fmt.Errorf("uvk5: handshake: %w", err)
		}
		sent++
		if err := sleepCtx(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	if err := sleepCtx(ctx, 200*time.Millisecond); err != nil {
		return err
	}
	f.drain()
	return nil
}

// drain drops queued frames without waiting.
func (f *flasher) drain() {
	for {
		select {
		case <-f.frames:
		default:
			return
		}
	}
}

// program sends every page and waits for each one's acknowledgement.
func (f *flasher) program(ctx context.Context, fw []byte, progress func(FlashProgress)) error {
	pages := (len(fw) + flashPage - 1) / flashPage
	ts := uint32(time.Now().UnixMilli())
	for page, tries := 0, 0; page < pages; {
		data := make([]byte, 12+flashPage) // ts, page, pages, 4 reserved, the page
		put32(data, ts)
		put16(data[4:], uint16(page))
		put16(data[6:], uint16(pages))
		copy(data[12:], fw[page*flashPage:min(len(fw), (page+1)*flashPage)])
		if _, err := f.rw.Write(EncodeFrame(msgProgFW, data, false)); err != nil {
			return fmt.Errorf("uvk5: write page %d: %w", page+1, err)
		}
		ok, err := f.pageAck(ctx, page)
		if err != nil {
			return err
		}
		if !ok {
			tries++
			if tries > f.t.PageRetries {
				return fmt.Errorf("uvk5: page %d of %d failed %d times; the radio stays in bootloader mode: flash again", page+1, pages, tries)
			}
			continue
		}
		tries = 0
		page++
		if progress != nil {
			progress(FlashProgress{Page: page, Pages: pages})
		}
	}
	return nil
}

// pageAck waits for page's acknowledgement: true when it was written,
// false on an error reply or no reply (send it again).
func (f *flasher) pageAck(ctx context.Context, page int) (bool, error) {
	deadline := time.Now().Add(f.t.PageReply)
	for {
		fr, err := f.next(ctx, deadline)
		if errors.Is(err, errTimeout) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if fr.ID != msgProgFWAck || len(fr.Data) < 8 || int(le16(fr.Data[4:])) != page {
			continue
		}
		return le16(fr.Data[6:]) == 0, nil
	}
}

// versionLess compares dotted versions ("7.00.07") numerically.
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionParts(v string) [3]int {
	var out [3]int
	for i, p := range strings.SplitN(v, ".", 3) {
		out[i], _ = strconv.Atoi(strings.TrimSpace(p))
	}
	return out
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
