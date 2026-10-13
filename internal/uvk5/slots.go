package uvk5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"regexp"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Firmware slots (F4HWN multiboot), ported from UV Studio's "Firmware
// Slots" (Apache-2.0, Armel FAUVEAU; internal/web/uvstudio/NOTICE). The
// radio keeps firmware images in external flash: slot 0 is its own backup
// of the main firmware (the radio refuses writes to it), slots 1-4 hold
// other firmware (the APRS one, an FM/voice one) to switch to from the
// radio's menu. Writing a slot runs with the radio in normal mode and
// never touches the firmware it is running.

const (
	cmdSlotInfo     uint16 = 0x0720
	cmdSlotErase    uint16 = 0x0722
	cmdSlotWrite    uint16 = 0x0724
	cmdSlotValidate uint16 = 0x0726

	// FirstSlot and LastSlot are the slots a host may write.
	FirstSlot = 1
	LastSlot  = 4
	// MaxSlotImage is the largest image (the radio's 118 KiB program area).
	MaxSlotImage = 0x1D800

	slotImageOffset = 0x1000 // the image follows the header sector
	slotChunk       = 128    // the radio's USB receive ring is 256 bytes
	slotHeaderSize  = 64
	slotMagic       = 0x31424D46 // "FMB1"
	slotHdrVersion  = 1
	slotCommitted   = 1
	slotNameLen     = 16

	slotEraseWait    = 30 * time.Second
	slotWriteWait    = 1500 * time.Millisecond
	slotValidateWait = 20 * time.Second
	slotWriteTries   = 5
)

// SlotStatus is the radio's answer about a slot (MB_OK / MB_ERR_*).
type SlotStatus uint8

const (
	SlotValid SlotStatus = iota
	SlotEmpty
	SlotNewerHeader
	SlotIncomplete
	SlotBadSize
	SlotBadCRC
	SlotFlashError
	SlotBadIndex
	SlotAuth
	SlotRAMLoad
	SlotProtected
)

var slotStatusText = []string{"valid", "empty", "header format too new", "incomplete", "bad size", "CRC mismatch",
	"external flash error", "no such slot", "refused (session timestamp)", "restore error", "protected"}

func (s SlotStatus) String() string {
	if int(s) < len(slotStatusText) {
		return slotStatusText[s]
	}
	return fmt.Sprintf("error %d", s)
}

// SlotInfo is one slot's header as the radio reports it.
type SlotInfo struct {
	Slot    int        `json:"slot"`
	Status  SlotStatus `json:"-"`
	State   string     `json:"state"` // Status in words
	Size    int        `json:"size,omitempty"`
	CRC32   uint32     `json:"crc32,omitempty"`
	Name    string     `json:"name,omitempty"`
	Version string     `json:"version,omitempty"`
}

// ErrNoSlots: the radio doesn't answer slot commands (its firmware has no
// multiboot).
var ErrNoSlots = errors.New("uvk5: the radio's firmware has no firmware slots")

// ValidSlotImage checks an image fits a slot.
func ValidSlotImage(img []byte) error {
	switch {
	case len(img) == 0:
		return errors.New("firmware file is empty")
	case len(img) > MaxSlotImage:
		return fmt.Errorf("firmware file is %d bytes; a slot holds at most %d", len(img), MaxSlotImage)
	}
	return nil
}

func validSlot(slot int) error {
	if slot < FirstSlot || slot > LastSlot {
		return fmt.Errorf("uvk5: slot %d: only slots %d-%d can be written", slot, FirstSlot, LastSlot)
	}
	return nil
}

// SlotInfo reads one slot's header (0 included: read-only).
func (s *Session) SlotInfo(ctx context.Context, slot int) (SlotInfo, error) {
	f, err := s.command(ctx, cmdSlotInfo, []byte{byte(slot)})
	if errors.Is(err, ErrNoReply) {
		return SlotInfo{}, ErrNoSlots
	}
	if err != nil {
		return SlotInfo{}, err
	}
	if len(f.Data) < 2+slotHeaderSize {
		return SlotInfo{}, fmt.Errorf("%w: slot info %d bytes", ErrFrame, len(f.Data))
	}
	info := SlotInfo{Slot: slot, Status: SlotStatus(f.Data[1])}
	info.State = info.Status.String()
	h := f.Data[2 : 2+slotHeaderSize]
	if binary.LittleEndian.Uint32(h) == slotMagic {
		info.Size = int(binary.LittleEndian.Uint32(h[8:]))
		info.CRC32 = binary.LittleEndian.Uint32(h[12:])
		info.Name, info.Version = cString(h[16:32]), cString(h[32:48])
	}
	return info, nil
}

func (s *Session) slotErase(ctx context.Context, slot int) (SlotStatus, error) {
	d := make([]byte, 6)
	d[0] = byte(slot)
	put32(d[2:], s.ts)
	f, err := s.commandWait(ctx, cmdSlotErase, d, slotEraseWait)
	return slotReply(f, err)
}

func (s *Session) slotWrite(ctx context.Context, slot, offset int, chunk []byte) (SlotStatus, error) {
	d := make([]byte, 12+len(chunk))
	d[0] = byte(slot)
	put32(d[2:], uint32(offset))
	put16(d[6:], uint16(len(chunk)))
	put32(d[8:], s.ts)
	copy(d[12:], chunk)
	var last error
	for range slotWriteTries { // rewriting the same bytes is harmless: a lost reply is retried
		f, err := s.commandWait(ctx, cmdSlotWrite, d, slotWriteWait)
		if err == nil || !errors.Is(err, ErrNoReply) {
			return slotReply(f, err)
		}
		last = err
	}
	return 0, last
}

func (s *Session) slotValidate(ctx context.Context, slot int) (uint32, SlotStatus, error) {
	f, err := s.commandWait(ctx, cmdSlotValidate, []byte{byte(slot)}, slotValidateWait)
	if err != nil {
		return 0, 0, err
	}
	if len(f.Data) < 6 {
		return 0, 0, fmt.Errorf("%w: validate reply %d bytes", ErrFrame, len(f.Data))
	}
	return binary.LittleEndian.Uint32(f.Data), SlotStatus(f.Data[5]), nil
}

func slotReply(f Frame, err error) (SlotStatus, error) {
	if err != nil {
		return 0, err
	}
	if len(f.Data) < 2 {
		return 0, fmt.Errorf("%w: slot reply %d bytes", ErrFrame, len(f.Data))
	}
	return SlotStatus(f.Data[1]), nil
}

// SlotProgress reports a slot write: bytes of the image written.
type SlotProgress struct {
	Done, Total int
	Step        string // erasing, writing, verifying
}

// WriteSlot writes img into slot (1-4) under name and version: erase,
// the image in chunks, then the header (which commits it), then the
// radio checks the CRC. A write that stops half way leaves the slot
// uncommitted (the radio won't boot it); the running firmware is never
// touched.
func (s *Session) WriteSlot(ctx context.Context, slot int, img []byte, name, version string, progress func(SlotProgress)) error {
	if err := validSlot(slot); err != nil {
		return err
	}
	if err := ValidSlotImage(img); err != nil {
		return err
	}
	report := func(p SlotProgress) {
		if progress != nil {
			p.Total = len(img)
			progress(p)
		}
	}
	report(SlotProgress{Step: "erasing"})
	st, err := s.slotErase(ctx, slot)
	if errors.Is(err, ErrNoReply) {
		return ErrNoSlots
	}
	if err != nil || st != SlotValid {
		return slotErr("erase", st, err)
	}
	for off := 0; off < len(img); off += slotChunk {
		chunk := img[off:min(off+slotChunk, len(img))]
		if st, err := s.slotWrite(ctx, slot, slotImageOffset+off, chunk); err != nil || st != SlotValid {
			return slotErr(fmt.Sprintf("write at %d", off), st, err)
		}
		report(SlotProgress{Step: "writing", Done: off + len(chunk)})
	}
	crc := crc32.ChecksumIEEE(img)
	if st, err := s.slotWrite(ctx, slot, 0, slotHeader(len(img), crc, name, version)); err != nil || st != SlotValid {
		return slotErr("header", st, err)
	}
	report(SlotProgress{Step: "verifying", Done: len(img)})
	got, st, err := s.slotValidate(ctx, slot)
	if err != nil || st != SlotValid {
		return slotErr("verify", st, err)
	}
	if got != crc {
		return fmt.Errorf("uvk5: verify: the radio read back CRC %08x, the file's is %08x", got, crc)
	}
	return nil
}

func slotErr(step string, st SlotStatus, err error) error {
	if err != nil {
		return fmt.Errorf("uvk5: slot %s: %w", step, err)
	}
	return fmt.Errorf("uvk5: slot %s: the radio says %s", step, st)
}

func slotHeader(size int, crc uint32, name, version string) []byte {
	h := make([]byte, slotHeaderSize)
	put32(h, slotMagic)
	put16(h[4:], slotHdrVersion)
	put16(h[6:], slotCommitted)
	put32(h[8:], uint32(size))
	put32(h[12:], crc)
	copy(h[16:16+slotNameLen-1], SlotName(name))
	copy(h[32:32+slotNameLen-1], asciiOnly(version))
	return h
}

// SlotName is a name the radio can show: ASCII, at most 15 characters
// (accents dropped, as UV Studio does).
func SlotName(s string) string {
	var b strings.Builder
	for _, r := range norm.NFD.String(s) {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		if r >= 0x20 && r < 0x7F {
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	if len(out) > slotNameLen-1 {
		out = out[:slotNameLen-1]
	}
	return out
}

func asciiOnly(s string) string {
	s = SlotName(s)
	return strings.ReplaceAll(s, " ", "")
}

var (
	editionRe = regexp.MustCompile(`(?i)^f4hwn[._-](?:(?:k1|k5v3)[._-])?([a-z][a-z0-9-]*)(?:[._-].*)?\.bin$`)
	versionRe = regexp.MustCompile(`(?i)[._-]v(\d+(?:\.\d+)*)\.bin$`)
	embedVer  = regexp.MustCompile(`(?i)[A-Za-z0-9]+ (v\d+(?:\.\d+)*)`)
)

// SlotMeta suggests a slot name and version from an F4HWN file name
// ("f4hwn.aprs.v6.1.bin" -> "Aprs", "v6.1"), else from the image's own
// version text, as UV Studio does.
func SlotMeta(filename string, img []byte) (name, version string) {
	if m := editionRe.FindStringSubmatch(filename); m != nil {
		var parts []string
		for _, p := range strings.Split(m[1], "-") {
			switch {
			case p == "":
			case strings.EqualFold(p, "fieldops"):
				parts = append(parts, "FieldOps")
			default:
				parts = append(parts, strings.ToUpper(p[:1])+strings.ToLower(p[1:]))
			}
		}
		name = SlotName(strings.Join(parts, " "))
	}
	if m := versionRe.FindStringSubmatch(filename); m != nil {
		version = "v" + m[1]
	} else {
		text := make([]byte, len(img))
		for i, c := range img {
			if c >= 32 && c < 127 {
				text[i] = c
			} else {
				text[i] = '\n'
			}
		}
		if m := embedVer.FindSubmatch(text); m != nil {
			version = string(m[1])
		}
	}
	return name, asciiOnly(version)
}
