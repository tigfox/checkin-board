package graywolf

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxPacketLimit bounds one packet-log read.
const maxPacketLimit = 1000

// PacketQuery filters GET /api/packets. Zero fields are omitted.
type PacketQuery struct {
	Since     time.Time
	Type      string // APRS packet type, e.g. "message"
	Direction string // "RX", "TX" or "IS"
	Limit     int
}

// Packet is a packet-log entry (webapi.packetDTO). Only the fields the
// link check uses are typed.
type Packet struct {
	Timestamp time.Time `json:"timestamp"`
	Direction string    `json:"direction"`
	Type      string    `json:"type"`
	// Via is the last digipeater that forwarded the frame; "" if direct.
	Via string `json:"via"`
	// AudioLevel is present only for frames heard via graywolf's own
	// modem (not TX, APRS-IS or a hardware TNC).
	AudioLevel *AudioLevel    `json:"audio_level"`
	Decoded    *DecodedPacket `json:"decoded"`
}

// AudioLevel is the demodulator's receive level for one frame.
type AudioLevel struct {
	LevelDBFS float64 `json:"level_dbfs"`
}

// DecodedPacket is the parsed APRS payload (aprs.DecodedAPRSPacket).
type DecodedPacket struct {
	Source  string         `json:"source"`
	Message *PacketMessage `json:"message"`
}

// PacketMessage is an APRS message payload (aprs.Message).
type PacketMessage struct {
	Addressee string `json:"addressee"`
	Text      string `json:"text"`
	MessageID string `json:"messageID"`
	IsAck     bool   `json:"isAck"`
}

// ListPackets calls GET /api/packets (graywolf's packet log).
func (c *Client) ListPackets(ctx context.Context, q PacketQuery) ([]Packet, error) {
	if q.Limit < 0 || q.Limit > maxPacketLimit {
		return nil, fmt.Errorf("graywolf: packet limit %d out of range 0..%d (0 = server default)", q.Limit, maxPacketLimit)
	}
	v := url.Values{}
	if !q.Since.IsZero() {
		v.Set("since", q.Since.UTC().Format(time.RFC3339))
	}
	if q.Type != "" {
		v.Set("type", q.Type)
	}
	if q.Direction != "" {
		v.Set("direction", q.Direction)
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out []Packet
	err := c.do(ctx, http.MethodGet, "/api/packets", v, nil, &out)
	return out, err
}
