package panel

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"
	"time"

	"checkin-board/internal/hostmon"
)

// The large status screen (phase 18), for a panel of at least
// LargeSize: the PaPiRus 2.7" (264x176) on the HQ Pi 3. Under the title
// bar: the node's own basics (address, CPU, temperature, uptime, its
// radio link), then on HQ one line per checkpoint with its last check-in
// time, so net control sees a quiet station at a glance. Times are clock
// times, not "ago", so the screen changes only when something happens.

// LargeSize is the smallest panel the large screen is drawn for.
var LargeSize = image.Pt(264, 176)

const (
	lgRowH    = 13 // small font rows
	lgInfoTop = headerH + 13
	// lgCode, lgName and lgTime are the checkpoint table's columns, in
	// small-font characters.
	lgCode = 6
	lgName = 16
)

// IsLarge reports whether a panel of size gets the large screen.
func IsLarge(size image.Point) bool { return size.X >= LargeSize.X && size.Y >= LargeSize.Y }

// LargeStatusScreen is the resting screen on a large panel.
func LargeStatusScreen(v View, addrs []string, health hostmon.Snapshot, size image.Point) *image.Gray {
	img := image.NewGray(image.Rectangle{Max: size})
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	st, now := v.Status, v.Status.Now
	maxBig, maxSm := size.X/charW, size.X/smallW

	// Title bar.
	draw.Draw(img, image.Rect(0, 0, size.X, headerH), image.Black, image.Point{}, draw.Src)
	right := clip(st.StateLabel, maxBig-2)
	text(img, 2, 14, clip(screenTitle(st), maxBig-len(right)-1), color.Gray{Y: 255})
	text(img, size.X-2-len(right)*charW, 14, right, color.Gray{Y: 255})

	// The node.
	row := func(i int, s string) {
		textIn(img, small, 2, lgInfoTop+i*lgRowH, clip(s, maxSm), color.Gray{})
	}
	addr := "No network address"
	if len(addrs) > 0 {
		addr = fmt.Sprintf("http://%s:%d", addrs[0], st.Port)
	}
	row(0, addr)
	row(1, nodeLine(health))
	row(2, linkLine(st, health))
	rule := lgInfoTop + 2*lgRowH + 4
	draw.Draw(img, image.Rect(0, rule, size.X, rule+1), image.Black, image.Point{}, draw.Src)

	// The body: warnings first, then the checkpoints (HQ) or delivery
	// (checkpoint).
	footerBase := size.Y - 3
	rows := (footerBase - lgRowH - (rule + 2)) / lgRowH
	var body []string
	if cpuHigh(health) {
		body = append(body, fmt.Sprintf("! CPU %.0f%% over 5 min", *health.CPUPercent))
	}
	for _, w := range st.Warnings {
		body = append(body, "! "+w)
	}
	switch st.Role {
	case "hq":
		body = append(body, checkpointRows(st.HQ, rows-len(body), maxSm)...)
	case "checkpoint":
		hq := "HQ not heard yet"
		if st.LastHQContact != nil {
			hq = "HQ last heard " + hhmm(*st.LastHQContact)
		}
		body = append(body, fmt.Sprintf("Unsent reports: %d", st.Unconfirmed), hq)
		if st.LastLink != nil {
			body = append(body, fmt.Sprintf("Link check %s %d/%d at %s", st.LastLink.Verdict, st.LastLink.Uplink, st.LastLink.Count, hhmm(st.LastLink.At)))
		}
	}
	for i, s := range body {
		if i >= rows {
			break
		}
		textIn(img, small, 2, rule+2+lgRowH*(i+1), clip(s, maxSm), color.Gray{})
	}

	// Footer.
	foot := "Updated " + hhmm(now)
	textIn(img, small, 2, footerBase, clip(st.RaceName, maxSm-len(foot)-2), color.Gray{})
	textIn(img, small, size.X-2-len(foot)*smallW, footerBase, foot, color.Gray{})
	return mono(img)
}

func screenTitle(st Status) string {
	switch {
	case st.Role == "checkpoint" && st.CPCode != "":
		return strings.TrimSpace(st.CPCode + " " + st.Station)
	case st.Role == "hq":
		return strings.TrimSpace("HQ " + st.Station)
	case st.Role == "":
		return "Not set up"
	}
	return st.Station
}

// nodeLine is the node's own health: CPU, temperature, uptime.
func nodeLine(h hostmon.Snapshot) string {
	var parts []string
	if h.CPUPercent != nil {
		parts = append(parts, fmt.Sprintf("CPU %.0f%%", *h.CPUPercent))
	}
	if h.CPUTempC != nil {
		parts = append(parts, fmt.Sprintf("%.0fC", *h.CPUTempC)) // ASCII: no degree sign in the font
	}
	if h.Uptime > 0 {
		parts = append(parts, "up "+uptime(h.Uptime))
	}
	if len(parts) == 0 {
		return "Node health not known yet"
	}
	return strings.Join(parts, "  ")
}

// uptime is a compact duration: "45m", "3h05m", "2d03h".
func uptime(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%02dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// linkLine is how race traffic goes out, and whether it can.
func linkLine(st Status, h hostmon.Snapshot) string {
	link := "graywolf"
	if st.Link == "radio" {
		link = "radio"
	}
	if !st.GraywolfOK {
		problem := strings.TrimPrefix(st.GraywolfProblem, "graywolf ")
		if problem == "" {
			problem = "not reachable"
		}
		return link + " DOWN: " + problem
	}
	s := link + " OK"
	if k := h.ModemKeepingUp; k != nil && !*k {
		s += ", modem BEHIND"
	}
	return s
}

// checkpointRows is HQ's table: code, name, last check-in, and closed or
// missing batches; at most n rows (the last says how many didn't fit).
func checkpointRows(hq *HQ, n, width int) []string {
	if hq == nil || n <= 0 {
		return nil
	}
	if len(hq.Checkpoints) == 0 {
		return []string{"No checkpoints yet"}
	}
	rows := []string{fmt.Sprintf("%-*s %-*s %s", lgCode, "CP", lgName, "Station", "Last in")}
	cps := hq.Checkpoints
	room := n - 1
	more := 0
	if len(cps) > room {
		more = len(cps) - (room - 1)
		cps = cps[:room-1]
	}
	for _, c := range cps {
		at := "--:--"
		if c.LastHeard != nil {
			at = hhmm(*c.LastHeard)
		}
		name, mark := c.Name, ""
		switch {
		case c.Unlisted:
			if name == "" {
				name = "(not on list)"
			} else {
				mark = " ?"
			}
		case c.Closed:
			mark = " closed"
		case c.Gaps > 0:
			mark = fmt.Sprintf(" gap %d", c.Gaps)
		}
		rows = append(rows, clip(fmt.Sprintf("%-*s %-*s %s%s", lgCode, clip(c.Code, lgCode), lgName, clip(name, lgName), at, mark), width))
	}
	if more > 0 {
		rows = append(rows, fmt.Sprintf("+%d more (Admin > HQ)", more))
	}
	return rows
}
