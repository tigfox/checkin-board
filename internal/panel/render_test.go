package panel

import (
	"bytes"
	"flag"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Golden screens: run with -update to rewrite testdata/*.png, then look
// at them. Every pixel must be pure black or white (a 1-bit panel).
var update = flag.Bool("update", false, "rewrite golden screen images")

var tNow = time.Date(2026, 10, 10, 13, 5, 0, 0, time.Local)

func ptr[T any](v T) *T { return &v }

func cpView() View {
	return View{Status: Status{
		Role: "checkpoint", RaceName: "Ridge 50K", RaceState: "active", StateLabel: "Checkpoint open",
		Station: "AID3", CPCode: "AS5", Unconfirmed: 3, LastHQContact: ptr(tNow.Add(-2 * time.Minute)),
		GraywolfOK: true, LastLink: &Link{Verdict: "PASS", At: tNow.Add(-75 * time.Minute), Uplink: 5, Count: 5},
		Port: 8090, Now: tNow,
	}}
}

func golden(t *testing.T, name string, img *image.Gray) {
	t.Helper()
	for _, p := range img.Pix {
		if p != 0 && p != 255 {
			t.Fatalf("%s: grey pixel %d (the panel is 1-bit)", name, p)
		}
	}
	if img.Bounds().Dx() != 250 || img.Bounds().Dy() != 122 {
		t.Fatalf("%s: size %v", name, img.Bounds())
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", name+".png")
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("%s: %v (run go test -update)", name, err)
	}
	defer f.Close()
	want, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	for y := range 122 {
		for x := range 250 {
			r1, _, _, _ := want.At(x, y).RGBA()
			if uint8(r1>>8) != img.GrayAt(x, y).Y {
				t.Fatalf("%s: differs from %s at (%d,%d); run go test -update and review the image", name, path, x, y)
			}
		}
	}
}

func TestScreens(t *testing.T) {
	hqv := View{Status: Status{
		Role: "hq", RaceName: "Ridge 50K", RaceState: "active", StateLabel: "Race active", Station: "NET",
		HQ: &HQ{Listed: 8, Heard: 6, Closed: 2, Gaps: 1}, GraywolfOK: false, GraywolfProblem: "graywolf unreachable",
		Warnings: []string{"Race clock not set"}, Port: 8090, Now: tNow,
	}}
	golden(t, "status-checkpoint", StatusScreen(cpView(), []string{"192.168.4.1"}))
	golden(t, "status-hq", StatusScreen(hqv, nil))
	golden(t, "menu", MenuScreen("Menu", []string{"Status", "Run link check", "Close checkpoint", "Secure for travel", "HQ check-in", "Network", "Refresh screen", "Back"}, 6))
	golden(t, "confirm", ConfirmScreen("Close checkpoint"))
	golden(t, "message", MessageScreen("Link check", []string{"Started: probing N0CALL-10.", "Result in 1-3 minutes."}))
	golden(t, "wizard", WizardScreen("ssd1680z", 0, 3))
	golden(t, "test-pattern", TestPattern("ssd1675"))
	golden(t, "app-down", AppDownScreen("connection refused", tNow))
}

func TestStatusAlwaysShowsAddress(t *testing.T) {
	v := cpView()
	v.Status.Warnings = []string{"one", "two", "three", "four", "five"}
	img := StatusScreen(v, []string{"10.1.2.3"})
	// The last body line is the address, even with many warnings: check
	// the address line isn't blank.
	base := bodyTop + (bodyLines-1)*lineH
	dark := 0
	for x := range 250 {
		for y := base - 10; y <= base; y++ {
			if img.GrayAt(x, y).Y == 0 {
				dark++
			}
		}
	}
	if dark == 0 {
		t.Fatal("address line is empty")
	}
}

func TestTextHelpers(t *testing.T) {
	if clip("abcdef", 4) != "abc~" || clip("ab", 4) != "ab" || clip("ab", 1) != "~" || clip("x", 0) != "" {
		t.Error("clip")
	}
	if ascii("Café→") != "Caf??" {
		t.Errorf("ascii = %q", ascii("Café→"))
	}
	for d, want := range map[time.Duration]string{30 * time.Second: "now", 4 * time.Minute: "4m", 125 * time.Minute: "2h05m"} {
		if got := ago(tNow.Add(-d), tNow); got != want {
			t.Errorf("ago(%v) = %q", d, got)
		}
	}
	r := Rotate(TestPattern("x"), 180)
	if r.GrayAt(249-5, 121-5) != TestPattern("x").GrayAt(5, 5) {
		t.Error("rotate")
	}
	if Rotate(TestPattern("x"), 0) == nil || !strings.Contains("x", "x") {
		t.Error("rotate 0")
	}
}

func TestBigTextFits(t *testing.T) {
	// Double-size characters are 14 px wide: 17 fit across 250 px.
	for _, s := range []string{"Readable? Then", "press a button.", "Test pattern"} {
		if len(s)*2*charW+3 > 250 {
			t.Errorf("%q is too wide for the panel", s)
		}
	}
}
