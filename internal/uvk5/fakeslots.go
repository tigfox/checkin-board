package uvk5

import (
	"encoding/binary"
	"hash/crc32"
)

// The fake radio's firmware slots (F4HWN multiboot) in "external flash",
// as the real firmware handles them: slot 0 refuses writes, erase and
// write need the session's timestamp, validate checks the CRC.

const fakeSlotSize = 0x20000

// fakeSlots is lazily made: a radio without them answers nothing.
type fakeSlots struct {
	flash  [LastSlot + 1][]byte
	writes int // chunk writes accepted, for tests
}

func (f *FakeRadio) slots() *fakeSlots {
	if f.mb == nil {
		f.mb = &fakeSlots{}
		for i := range f.mb.flash {
			f.mb.flash[i] = make([]byte, fakeSlotSize)
			for j := range f.mb.flash[i] {
				f.mb.flash[i][j] = 0xFF
			}
		}
	}
	return f.mb
}

// SetSlots turns the multiboot slot commands on or off.
func (f *FakeRadio) SetSlots(on bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.noSlots = !on
}

// Slot returns slot's header info and image as stored.
func (f *FakeRadio) Slot(slot int) (SlotInfo, []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	mem := f.slots().flash[slot]
	st, info := slotState(slot, mem)
	info.Status, info.State = st, st.String()
	var img []byte
	if info.Size > 0 && info.Size <= MaxSlotImage {
		img = append([]byte(nil), mem[slotImageOffset:slotImageOffset+info.Size]...)
	}
	return info, img
}

// SlotWrites counts accepted chunk writes.
func (f *FakeRadio) SlotWrites() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.slots().writes
}

func slotState(slot int, mem []byte) (SlotStatus, SlotInfo) {
	info := SlotInfo{Slot: slot}
	h := mem[:slotHeaderSize]
	if binary.LittleEndian.Uint32(h) != slotMagic {
		return SlotEmpty, info
	}
	info.Size = int(binary.LittleEndian.Uint32(h[8:]))
	info.CRC32 = binary.LittleEndian.Uint32(h[12:])
	info.Name, info.Version = cString(h[16:32]), cString(h[32:48])
	switch {
	case binary.LittleEndian.Uint16(h[6:])&slotCommitted == 0:
		return SlotIncomplete, info
	case info.Size <= 0 || info.Size > MaxSlotImage:
		return SlotBadSize, info
	case crc32.ChecksumIEEE(mem[slotImageOffset:slotImageOffset+info.Size]) != info.CRC32:
		return SlotBadCRC, info
	}
	return SlotValid, info
}

// handleSlot answers a slot command (caller holds f.mu); ok false: not a
// slot command, or slots are off.
func (f *FakeRadio) handleSlot(c Frame) ([]byte, bool) {
	if f.noSlots {
		return nil, false
	}
	d := c.Data
	if len(d) < 1 {
		return nil, false
	}
	slot := int(d[0])
	mb := f.slots()
	inRange := slot >= 0 && slot <= LastSlot
	authOK := func(at int) bool { return len(d) >= at+4 && f.ts != 0 && le32(d[at:]) == f.ts }
	reply := func(st SlotStatus) []byte { return []byte{byte(slot), byte(st)} }
	switch c.ID {
	case cmdSlotInfo:
		out := make([]byte, 2+slotHeaderSize)
		out[0] = byte(slot)
		if !inRange {
			out[1] = byte(SlotBadIndex)
			return out, true
		}
		st, _ := slotState(slot, mb.flash[slot])
		out[1] = byte(st)
		copy(out[2:], mb.flash[slot][:slotHeaderSize])
		return out, true
	case cmdSlotErase:
		switch {
		case !authOK(2):
			return reply(SlotAuth), true
		case !inRange:
			return reply(SlotBadIndex), true
		case slot == 0:
			return reply(SlotProtected), true
		}
		for i := range mb.flash[slot] {
			mb.flash[slot][i] = 0xFF
		}
		return reply(SlotValid), true
	case cmdSlotWrite:
		if len(d) < 12 {
			return nil, false
		}
		off, n := int(le32(d[2:])), int(le16(d[6:]))
		switch {
		case !authOK(8):
			return reply(SlotAuth), true
		case !inRange:
			return reply(SlotBadIndex), true
		case slot == 0:
			return reply(SlotProtected), true
		case n > len(d)-12 || off+n > fakeSlotSize:
			return reply(SlotBadSize), true
		}
		for i := range n { // NOR flash: programming only clears bits
			mb.flash[slot][off+i] &= d[12+i]
		}
		mb.writes++
		return reply(SlotValid), true
	case cmdSlotValidate:
		out := make([]byte, 6)
		out[4] = byte(slot)
		if !inRange {
			out[5] = byte(SlotBadIndex)
			return out, true
		}
		st, info := slotState(slot, mb.flash[slot])
		if info.Size > 0 && info.Size <= MaxSlotImage {
			put32(out, crc32.ChecksumIEEE(mb.flash[slot][slotImageOffset:slotImageOffset+info.Size]))
		}
		out[5] = byte(st)
		return out, true
	}
	return nil, false
}
