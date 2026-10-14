package epd

import "image"

// PaPiRus (PiSupply) HATs: Pervasive Displays e-paper panels with the
// G2 COG (V231_G2), in five sizes. The size can't be read from the panel,
// so each size is its own "controller" name.
const (
	PaPiRus144 = "papirus-1.44"
	PaPiRus19  = "papirus-1.9"
	PaPiRus20  = "papirus-2.0"
	PaPiRus26  = "papirus-2.6"
	PaPiRus27  = "papirus-2.7"
)

// PaPiRusSizes lists the PaPiRus names, largest first.
func PaPiRusSizes() []string { return []string{PaPiRus27, PaPiRus26, PaPiRus20, PaPiRus19, PaPiRus144} }

// IsPaPiRus reports whether c names a PaPiRus panel.
func IsPaPiRus(c string) bool {
	_, ok := g2Sizes[c]
	return ok
}

// Size is the panel's landscape size for controller c (the bonnet's for
// its controllers).
func Size(c string) image.Point {
	if g, ok := g2Sizes[c]; ok {
		return image.Pt(g.dots, g.lines)
	}
	return image.Pt(Width, Height)
}

// g2Size is one panel size's geometry and timing (gratis epd.c,
// EPD_create).
type g2Size struct {
	lines, dots   int // lines_per_display, dots_per_line
	scanBytes     int // bytes_per_scan
	middleScan    bool
	preBorderByte bool
	border        g2Border
	stageMS       int // base_stage_time
	channelSelect []byte
	pulseBorder   bool // the 2.7": pulse the border pin at the end
}

type g2Border int

const (
	borderNone g2Border = iota
	borderZero
	borderSet
)

var g2Sizes = map[string]g2Size{
	PaPiRus144: {lines: 96, dots: 128, scanBytes: 96 / 4, middleScan: true, border: borderZero, stageMS: 480,
		channelSelect: []byte{0x72, 0x00, 0x00, 0x00, 0x00, 0x00, 0x0f, 0xff, 0x00}},
	PaPiRus19: {lines: 128, dots: 144, scanBytes: 128 / 4 / 2, border: borderSet, stageMS: 480,
		channelSelect: []byte{0x72, 0x00, 0x00, 0x00, 0x03, 0xfc, 0x00, 0x00, 0xff}},
	PaPiRus20: {lines: 96, dots: 200, scanBytes: 96 / 4, middleScan: true, preBorderByte: true, border: borderNone, stageMS: 480,
		channelSelect: []byte{0x72, 0x00, 0x00, 0x00, 0x00, 0x01, 0xff, 0xe0, 0x00}},
	PaPiRus26: {lines: 128, dots: 232, scanBytes: 128 / 4 / 2, border: borderSet, stageMS: 630,
		channelSelect: []byte{0x72, 0x00, 0x00, 0x1f, 0xe0, 0x00, 0x00, 0x00, 0xff}},
	PaPiRus27: {lines: 176, dots: 264, scanBytes: 176 / 4, middleScan: true, preBorderByte: true, border: borderNone, stageMS: 630,
		channelSelect: []byte{0x72, 0x00, 0x00, 0x00, 0x7f, 0xff, 0xfe, 0x00, 0x00}, pulseBorder: true},
}
