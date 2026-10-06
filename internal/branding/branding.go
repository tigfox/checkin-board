// Package branding validates the HQ status board's branding (spec 8.3):
// header and footer text, a colour scheme whose readability is enforced
// (WCAG contrast), and an uploaded logo that is decoded, size-checked,
// scaled and re-encoded so the uploaded bytes never reach a browser.
package branding

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"checkin-board/internal/store"
)

// Limits and defaults.
const (
	MaxHeaderLen = 80
	MaxFooterLen = 200

	// MinTextContrast is WCAG AA for normal text; MinUIContrast is the
	// WCAG minimum for non-text elements (the accent highlights).
	MinTextContrast = 4.5
	MinUIContrast   = 3.0
)

// Default is the app's neutral, high-contrast board theme.
var Default = Colors{Primary: "#1F3A5F", Accent: "#B85C00", Background: "#FFFFFF", Text: "#111111"}

var hexColorRe = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// ErrInvalid wraps every validation failure (HTTP 400).
var ErrInvalid = errors.New("branding: invalid")

// Colors is the board's colour scheme: the header bar (Primary, with
// Background-coloured text on it), highlights such as the latest
// passage (Accent), and the page (Text on Background).
type Colors struct {
	Primary    string `json:"primary"`
	Accent     string `json:"accent"`
	Background string `json:"background"`
	Text       string `json:"text"`
}

// Settings is an edit from the admin page. Empty colours mean the
// default; an empty header means the race name.
type Settings struct {
	HeaderText string `json:"header_text"`
	FooterText string `json:"footer_text"`
	Colors     Colors `json:"colors"`
}

// Contrast is one checked pair, for the editor's readout.
type Contrast struct {
	Pair  string  `json:"pair"`
	Ratio float64 `json:"ratio"`
	Min   float64 `json:"min"`
	OK    bool    `json:"ok"`
}

// ValidationError lists every problem, with the contrast ratios.
type ValidationError struct {
	Problems  []string
	Contrasts []Contrast
}

func (e *ValidationError) Error() string {
	return "branding: " + strings.Join(e.Problems, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

// Effective returns c with empty colours filled from Default.
func (c Colors) Effective() Colors {
	fill := func(v, d string) string {
		if v == "" {
			return d
		}
		return strings.ToUpper(v)
	}
	return Colors{
		Primary:    fill(c.Primary, Default.Primary),
		Accent:     fill(c.Accent, Default.Accent),
		Background: fill(c.Background, Default.Background),
		Text:       fill(c.Text, Default.Text),
	}
}

// Check validates s and returns its normalized form and the contrast
// readout. On failure the error is a *ValidationError (which also
// carries the readout, for the editor).
func Check(s Settings) (Settings, []Contrast, error) {
	var problems []string
	out := Settings{HeaderText: strings.TrimSpace(s.HeaderText), FooterText: strings.TrimSpace(s.FooterText)}
	if msg := checkText("header", out.HeaderText, MaxHeaderLen); msg != "" {
		problems = append(problems, msg)
	}
	if msg := checkText("footer", out.FooterText, MaxFooterLen); msg != "" {
		problems = append(problems, msg)
	}
	for name, v := range map[string]*string{
		"primary": &s.Colors.Primary, "accent": &s.Colors.Accent,
		"background": &s.Colors.Background, "text": &s.Colors.Text,
	} {
		if *v != "" && !hexColorRe.MatchString(*v) {
			problems = append(problems, fmt.Sprintf("%s colour %q must be #RRGGBB", name, *v))
		}
	}
	if len(problems) > 0 {
		return Settings{}, nil, &ValidationError{Problems: problems}
	}
	norm := func(v string) string { return strings.ToUpper(v) }
	out.Colors = Colors{Primary: norm(s.Colors.Primary), Accent: norm(s.Colors.Accent),
		Background: norm(s.Colors.Background), Text: norm(s.Colors.Text)}

	eff := out.Colors.Effective()
	contrasts := []Contrast{
		pair("text on background", eff.Text, eff.Background, MinTextContrast),
		pair("header text on primary", eff.Background, eff.Primary, MinTextContrast),
		pair("accent on background", eff.Accent, eff.Background, MinUIContrast),
	}
	for _, c := range contrasts {
		if !c.OK {
			problems = append(problems, fmt.Sprintf("%s contrast %.1f:1 is below %.1f:1", c.Pair, c.Ratio, c.Min))
		}
	}
	if len(problems) > 0 {
		return Settings{}, contrasts, &ValidationError{Problems: problems, Contrasts: contrasts}
	}
	return out, contrasts, nil
}

func pair(name, fg, bg string, minRatio float64) Contrast {
	r := ContrastRatio(fg, bg)
	return Contrast{Pair: name, Ratio: math.Round(r*100) / 100, Min: minRatio, OK: r >= minRatio}
}

// checkText applies the name rules: printable, no control or
// bidirectional-override characters (which can make text render as
// something else), at most maxLen characters. Empty is allowed.
func checkText(field, s string, maxLen int) string {
	if !utf8.ValidString(s) {
		return field + " text is not valid UTF-8"
	}
	if utf8.RuneCountInString(s) > maxLen {
		return fmt.Sprintf("%s text is longer than %d characters", field, maxLen)
	}
	for _, r := range s {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return field + " text contains a control or direction-override character"
		}
	}
	return ""
}

// ContrastRatio is the WCAG 2 contrast ratio of two #RRGGBB colours
// (1 to 21).
func ContrastRatio(a, b string) float64 {
	la, lb := luminance(a), luminance(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}

func luminance(hex string) float64 {
	var r, g, b uint8
	_, _ = fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%02x%02x%02x", &r, &g, &b)
	ch := func(v uint8) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*ch(r) + 0.7152*ch(g) + 0.0722*ch(b)
}

// FromRow converts stored branding to Settings.
func FromRow(r store.BrandingRow) Settings {
	return Settings{HeaderText: r.HeaderText, FooterText: r.FooterText, Colors: Colors{
		Primary: r.ColorPrimary, Accent: r.ColorAccent, Background: r.ColorBackground, Text: r.ColorText,
	}}
}

// ToRow converts checked Settings for storage.
func (s Settings) ToRow() store.BrandingRow {
	return store.BrandingRow{HeaderText: s.HeaderText, FooterText: s.FooterText,
		ColorPrimary: s.Colors.Primary, ColorAccent: s.Colors.Accent,
		ColorBackground: s.Colors.Background, ColorText: s.Colors.Text}
}
