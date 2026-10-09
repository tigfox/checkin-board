// Package epd drives the node panel's e-ink display (spec 8.4). Bonnet
// revisions use different controllers; the panel detects which.
package epd

import "image"

// Controllers the Adafruit 2.13" bonnet revisions use.
const (
	SSD1680Z = "ssd1680z" // current revision
	SSD1680  = "ssd1680"  // "legacy" SSD1680 revision
	SSD1675  = "ssd1675"  // original revision
)

// Controllers lists them, newest bonnet first (the detection order).
func Controllers() []string { return []string{SSD1680Z, SSD1680, SSD1675} }

// Known reports whether c names a controller.
func Known(c string) bool {
	for _, k := range Controllers() {
		if c == k {
			return true
		}
	}
	return false
}

// Panel size, landscape.
const (
	Width  = 250
	Height = 122
)

// Display is one e-ink panel.
type Display interface {
	// Full redraws the whole panel (slow, clears ghosting).
	Full(img image.Image) error
	// Partial updates it quickly; only if CanPartial.
	Partial(img image.Image) error
	CanPartial() bool
	Close() error
}
