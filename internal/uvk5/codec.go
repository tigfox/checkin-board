package uvk5

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"
)

// key obfuscates every frame's payload and CRC (spec section 3).
var key = [16]byte{0x16, 0x6C, 0x14, 0xE6, 0x2E, 0x91, 0x0D, 0x40, 0x21, 0x35, 0xD5, 0x40, 0x13, 0x03, 0xE9, 0x80}

const (
	maxFrameSize = 1024 // payload bytes; real frames are under 120
	maxLineLen   = 1024
)

// ErrFrame is a frame that can't be read (bad header, size or trailer).
var ErrFrame = errors.New("uvk5: bad frame")

// Frame is one decoded command or reply.
type Frame struct {
	ID   uint16
	Data []byte
	// CRCOK: the CRC slot holds the payload's CRC-16/XMODEM, as on a
	// host command (replies carry FF FF instead).
	CRCOK bool
}

// crc16 is CRC-16/XMODEM: poly 0x1021, init 0, no reflection.
func crc16(b []byte) uint16 {
	var crc uint16
	for _, c := range b {
		crc ^= uint16(c) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// EncodeFrame builds a frame as the host sends it: payload (id, len,
// data) and its CRC, obfuscated, between AB CD + size and DC BA. With
// reply true the CRC slot carries the radio's FF FF marker instead.
func EncodeFrame(id uint16, data []byte, reply bool) []byte {
	payload := make([]byte, 4+len(data))
	put16(payload, id)
	put16(payload[2:], uint16(len(data)))
	copy(payload[4:], data)
	body := make([]byte, len(payload)+2)
	copy(body, payload)
	if reply {
		body[len(payload)], body[len(payload)+1] = 0xFF, 0xFF
	} else {
		put16(body[len(payload):], crc16(payload))
	}
	for i := range body {
		body[i] ^= key[i%len(key)]
	}
	out := make([]byte, 0, len(body)+6)
	out = append(out, 0xAB, 0xCD, byte(len(payload)), byte(len(payload)>>8))
	out = append(out, body...)
	return append(out, 0xDC, 0xBA)
}

// Item is one thing read from the radio: a frame, or a text line.
type Item struct {
	Frame *Frame
	Line  string
}

// Reader splits the radio's stream into frames and monitor lines.
type Reader struct {
	br *bufio.Reader
}

// NewReader reads from r.
func NewReader(r io.Reader) *Reader { return &Reader{br: bufio.NewReaderSize(r, 4096)} }

// Next returns the next frame or line. A malformed frame is reported
// as ErrFrame (wrapped) and skipped; reading can go on.
func (r *Reader) Next() (Item, error) {
	b, err := r.br.ReadByte()
	if err != nil {
		return Item{}, err
	}
	if b == 0xAB {
		if next, err := r.br.Peek(1); err == nil && next[0] == 0xCD {
			_, _ = r.br.ReadByte()
			f, err := r.readFrame()
			if err != nil {
				return Item{}, err
			}
			return Item{Frame: &f}, nil
		}
	}
	if b == '\n' || b == '\r' {
		return r.Next() // a stray line end
	}
	line := []byte{b}
	for len(line) < maxLineLen {
		c, err := r.br.ReadByte()
		if err != nil {
			return Item{}, err
		}
		if c == '\n' {
			break
		}
		line = append(line, c)
	}
	return Item{Line: strings.TrimRight(string(line), "\r")}, nil
}

func (r *Reader) readFrame() (Frame, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r.br, hdr[:]); err != nil {
		return Frame{}, err
	}
	size := int(le16(hdr[:]))
	if size < 4 || size > maxFrameSize {
		return Frame{}, fmt.Errorf("%w: size %d", ErrFrame, size)
	}
	rest := make([]byte, size+4)
	if _, err := io.ReadFull(r.br, rest); err != nil {
		return Frame{}, err
	}
	if rest[size+2] != 0xDC || rest[size+3] != 0xBA {
		return Frame{}, fmt.Errorf("%w: no DC BA trailer", ErrFrame)
	}
	body := rest[:size+2]
	for i := range body {
		body[i] ^= key[i%len(key)]
	}
	payload := body[:size]
	n := int(le16(payload[2:]))
	if n > size-4 {
		return Frame{}, fmt.Errorf("%w: data length %d in a %d-byte payload", ErrFrame, n, size)
	}
	return Frame{ID: le16(payload), Data: append([]byte(nil), payload[4:4+n]...),
		CRCOK: le16(body[size:]) == crc16(payload)}, nil
}
