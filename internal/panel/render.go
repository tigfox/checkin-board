package panel

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/inconsolata"
	"golang.org/x/image/math/fixed"

	"checkin-board/internal/hostmon"
	"checkin-board/internal/panel/epd"
)

// Screens are 1-bit images (black text on white) the size of the panel.
// Header, body and menus use a bold 8×16 bitmap font (31 characters a
// line), readable on the 2.13" panel; the footer and button hints use
// a small 7×13 one (35 characters).

const (
	charW    = 8
	smallW   = 7
	lineH    = 17
	headerH  = 18
	maxChars = epd.Width / charW
	maxSmall = epd.Width / smallW
	// bodyTop is the first body line's baseline; five lines fit above
	// the footer.
	bodyTop    = headerH + 16
	bodyLines  = 5
	footerBase = epd.Height - 3
)

var (
	face  font.Face = inconsolata.Bold8x16
	small font.Face = basicfont.Face7x13
)

// mono thresholds a finished screen to pure black and white (the font's
// edges carry grey levels the panel can't show).
func mono(img *image.Gray) *image.Gray {
	for i, v := range img.Pix {
		if v < 128 {
			img.Pix[i] = 0
		} else {
			img.Pix[i] = 255
		}
	}
	return img
}

func newCanvas() *image.Gray {
	img := image.NewGray(image.Rect(0, 0, epd.Width, epd.Height))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	return img
}

// clip shortens s to n characters, marking the cut with "~".
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	s = ascii(s) // one byte per drawn character from here on
	if len(s) <= n {
		return s
	}
	if n == 1 {
		return "~"
	}
	return s[:n-1] + "~"
}

// ascii keeps the printable ASCII the font has (other runes become '?').
func ascii(s string) string {
	b := []byte(s)
	out := b[:0]
	for _, r := range s {
		if r >= 0x20 && r <= 0x7e {
			out = append(out, byte(r))
		} else {
			out = append(out, '?')
		}
	}
	return string(out)
}

func text(img *image.Gray, x, baseline int, s string, c color.Gray) {
	textIn(img, face, x, baseline, s, c)
}

func textIn(img *image.Gray, f font.Face, x, baseline int, s string, c color.Gray) {
	d := font.Drawer{Dst: img, Src: image.NewUniform(c), Face: f, Dot: fixed.P(x, baseline)}
	d.DrawString(ascii(s))
}

// bigText draws s at twice the size, for messages read from a distance.
func bigText(img *image.Gray, x, top int, s string) {
	src := image.NewGray(image.Rect(0, 0, len(s)*smallW, 13))
	draw.Draw(src, src.Bounds(), image.White, image.Point{}, draw.Src)
	textIn(src, small, 0, 11, s, color.Gray{})
	for y := range 13 {
		for x2 := range src.Bounds().Dx() {
			c := src.GrayAt(x2, y)
			for dy := range 2 {
				for dx := range 2 {
					img.SetGray(x+2*x2+dx, top+2*y+dy, c)
				}
			}
		}
	}
}

// header draws the black title bar, left and right texts in white.
func header(img *image.Gray, left, right string) {
	draw.Draw(img, image.Rect(0, 0, epd.Width, headerH), image.Black, image.Point{}, draw.Src)
	right = clip(right, maxChars-2)
	left = clip(left, maxChars-len(right)-1)
	text(img, 2, 14, left, color.Gray{Y: 255})
	text(img, epd.Width-2-len(right)*charW, 14, right, color.Gray{Y: 255})
}

func line(img *image.Gray, i int, s string) {
	if i < bodyLines {
		text(img, 2, bodyTop+i*lineH, clip(s, maxChars), color.Gray{})
	}
}

func footer(img *image.Gray, left, right string) {
	right = clip(right, maxSmall-1)
	textIn(img, small, 2, footerBase, clip(left, maxSmall-len(right)-2), color.Gray{})
	textIn(img, small, epd.Width-2-len(right)*smallW, footerBase, right, color.Gray{})
}

// ago is a compact age: "now", "4m", "2h05m".
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

func hhmm(t time.Time) string { return t.Local().Format("15:04") }

// cpuWarnPercent is the 5-minute CPU average that becomes a warning
// line: a Pi Zero runs ~55-65% at 24 kHz, and graywolf's modem falls
// behind when it can't get its share (feedback 2026-10-09, item 13).
const cpuWarnPercent = 80

// StatusScreen is the panel's resting screen. addrs are the node's
// network addresses and health its CPU and radio modem (the panel reads
// both itself; the app can't).
func StatusScreen(v View, addrs []string, health hostmon.Snapshot) *image.Gray {
	img := newCanvas()
	st := v.Status
	title := st.Station
	switch {
	case st.Role == "checkpoint" && st.CPCode != "":
		title = strings.TrimSpace(st.CPCode + " " + st.Station)
	case st.Role == "hq":
		title = strings.TrimSpace("HQ " + st.Station)
	case st.Role == "":
		title = "Not set up"
	}
	header(img, title, st.StateLabel)
	lines, right := statusLines(v, addrs, health)
	for i, l := range lines {
		line(img, i, l)
	}
	footer(img, st.RaceName, right)
	return mono(img)
}

// statusLines is the status screen's body and footer text.
func statusLines(v View, addrs []string, health hostmon.Snapshot) (lines []string, footerRight string) {
	st, now := v.Status, v.Status.Now
	switch st.Role {
	case "checkpoint":
		hq := "HQ not heard yet"
		if st.LastHQContact != nil {
			hq = "HQ heard " + ago(*st.LastHQContact, now) + " ago"
		}
		lines = append(lines, fmt.Sprintf("Unsent %d, %s", st.Unconfirmed, hq))
	case "hq":
		if st.HQ != nil {
			lines = append(lines, fmt.Sprintf("Heard %d/%d checkpoints, %d closed", st.HQ.Heard, st.HQ.Listed, st.HQ.Closed))
			if st.HQ.Gaps > 0 {
				lines = append(lines, fmt.Sprintf("Missing batches: %d", st.HQ.Gaps))
			}
		}
	}
	if st.GraywolfOK {
		gw := "graywolf OK"
		if k := health.ModemKeepingUp; k != nil && *k {
			gw += ", modem keeping up"
		} else if k != nil {
			gw += ", modem BEHIND"
		}
		lines = append(lines, gw)
	} else {
		problem := strings.TrimPrefix(st.GraywolfProblem, "graywolf ")
		if problem == "" {
			problem = "not reachable"
		}
		lines = append(lines, "graywolf DOWN: "+problem)
	}
	// Warnings outrank the link line when space runs short.
	if cpuHigh(health) {
		lines = append(lines, fmt.Sprintf("! CPU %.0f%% over 5 min", *health.CPUPercent))
	}
	for _, w := range st.Warnings {
		lines = append(lines, "! "+w)
	}
	if st.LastLink != nil {
		lines = append(lines, fmt.Sprintf("Link %s %d/%d, %s ago", st.LastLink.Verdict, st.LastLink.Uplink, st.LastLink.Count, ago(st.LastLink.At, now)))
	} else {
		lines = append(lines, "Link not checked yet")
	}
	addr := "No network address"
	if len(addrs) > 0 {
		addr = fmt.Sprintf("http://%s:%d", addrs[0], st.Port)
	}
	// The address always shows: it's how volunteers reach the keypad.
	if len(lines) > bodyLines-1 {
		lines = lines[:bodyLines-1]
	}
	lines = append(lines, addr)
	footerRight = "Updated " + hhmm(now)
	if health.CPUPercent != nil {
		footerRight = fmt.Sprintf("CPU %.0f%% | %s", *health.CPUPercent, hhmm(now)) // ASCII: the panel font has no "·"
	}
	return lines, footerRight
}

func cpuHigh(h hostmon.Snapshot) bool { return h.CPUPercent != nil && *h.CPUPercent >= cpuWarnPercent }

// modemBehind is a measured "falling behind", not an unknown.
func modemBehind(h hostmon.Snapshot) bool { return h.ModemKeepingUp != nil && !*h.ModemKeepingUp }

// MenuScreen lists labels with the cursor's item highlighted.
func MenuScreen(title string, labels []string, cursor int) *image.Gray {
	img := newCanvas()
	header(img, title, fmt.Sprintf("%d/%d", cursor+1, len(labels)))
	first := 0
	if cursor >= bodyLines {
		first = cursor - bodyLines + 1
	}
	for i := first; i < len(labels) && i-first < bodyLines; i++ {
		row := i - first
		base := bodyTop + row*lineH
		if i == cursor {
			draw.Draw(img, image.Rect(0, base-14, epd.Width, base+3), image.Black, image.Point{}, draw.Src)
			text(img, 2, base, "> "+clip(labels[i], maxChars-2), color.Gray{Y: 255})
		} else {
			text(img, 2, base, "  "+clip(labels[i], maxChars-2), color.Gray{})
		}
	}
	footer(img, "TOP: next", "BOTTOM: select")
	return mono(img)
}

// ConfirmScreen asks before running an item.
func ConfirmScreen(label string) *image.Gray {
	img := newCanvas()
	header(img, "Confirm", "")
	line(img, 0, label+"?")
	line(img, 2, "BOTTOM: yes, do it")
	line(img, 3, "TOP: cancel")
	return mono(img)
}

// MessageScreen shows a title and a few lines (results, details).
func MessageScreen(title string, lines []string) *image.Gray {
	img := newCanvas()
	header(img, title, "")
	for i, l := range lines {
		line(img, i, l)
	}
	return mono(img)
}

// WizardScreen is shown with each candidate controller while finding
// the bonnet's: only the right one draws readably.
func WizardScreen(controller string, i, n int) *image.Gray {
	img := newCanvas()
	header(img, "Display setup", fmt.Sprintf("%d/%d", i+1, n))
	// Double-size text fits 17 characters a line.
	bigText(img, 3, 22, "Readable? Then")
	bigText(img, 3, 52, "press a button.")
	footer(img, "Trying "+controller, "")
	return mono(img)
}

// WizardLikeScreen shows two lines of double-size text (bench screens
// read from a distance).
func WizardLikeScreen(a, b string) *image.Gray {
	img := newCanvas()
	bigText(img, 3, 22, clip(a, 17))
	bigText(img, 3, 62, clip(b, 17))
	return mono(img)
}

// TestPattern helps check a controller by eye: a border, corner marks
// and the controller's name.
func TestPattern(controller string) *image.Gray {
	img := newCanvas()
	b := img.Bounds()
	for x := range b.Dx() {
		img.SetGray(x, 0, color.Gray{})
		img.SetGray(x, b.Dy()-1, color.Gray{})
	}
	for y := range b.Dy() {
		img.SetGray(0, y, color.Gray{})
		img.SetGray(b.Dx()-1, y, color.Gray{})
	}
	for _, c := range []image.Point{{2, 2}, {b.Dx() - 22, 2}, {2, b.Dy() - 22}, {b.Dx() - 22, b.Dy() - 22}} {
		draw.Draw(img, image.Rect(c.X, c.Y, c.X+20, c.Y+20), image.Black, image.Point{}, draw.Src)
	}
	bigText(img, 30, 30, "Test pattern")
	text(img, 30, 84, "controller: "+controller, color.Gray{})
	return mono(img)
}

// AppDownScreen says the app isn't answering (the panel runs apart).
func AppDownScreen(problem string, now time.Time) *image.Gray {
	img := newCanvas()
	header(img, "checkin-board", "")
	line(img, 0, "The app isn't responding.")
	line(img, 1, problem)
	line(img, 3, "Reports are kept and sent")
	line(img, 4, "once it's back.")
	footer(img, "", "Updated "+hhmm(now))
	return mono(img)
}

// Rotate returns img turned by deg (0 or 180).
func Rotate(img *image.Gray, deg int) *image.Gray {
	if deg != 180 {
		return img
	}
	b := img.Bounds()
	out := image.NewGray(b)
	for y := range b.Dy() {
		for x := range b.Dx() {
			out.SetGray(b.Dx()-1-x, b.Dy()-1-y, img.GrayAt(x, y))
		}
	}
	return out
}
