package branding

import (
	"bytes"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
	"testing"

	"checkin-board/internal/store"
)

func TestContrastRatio(t *testing.T) {
	cases := []struct {
		a, b string
		want float64
	}{
		{"#000000", "#FFFFFF", 21},
		{"#FFFFFF", "#FFFFFF", 1},
		{"#777777", "#FFFFFF", 4.48}, // the classic just-fails-AA grey
	}
	for _, c := range cases {
		if got := ContrastRatio(c.a, c.b); math.Abs(got-c.want) > 0.01 {
			t.Errorf("ContrastRatio(%s, %s) = %.3f, want %.2f", c.a, c.b, got, c.want)
		}
	}
}

func TestDefaultThemePasses(t *testing.T) {
	if _, cs, err := Check(Settings{}); err != nil {
		t.Fatalf("default theme fails its own rules: %v %+v", err, cs)
	}
}

func TestCheckNormalizesAndReportsContrast(t *testing.T) {
	got, cs, err := Check(Settings{HeaderText: "  Ridge 50K  ", Colors: Colors{Primary: "#0b3d91", Text: "#000000"}})
	if err != nil {
		t.Fatal(err)
	}
	if got.HeaderText != "Ridge 50K" || got.Colors.Primary != "#0B3D91" || got.Colors.Accent != "" {
		t.Fatalf("normalized = %+v", got)
	}
	if len(cs) != 3 || !cs[0].OK || cs[0].Ratio < 20 {
		t.Fatalf("contrasts = %+v", cs)
	}
}

func TestCheckRejects(t *testing.T) {
	cases := map[string]Settings{
		"long header":   {HeaderText: strings.Repeat("x", MaxHeaderLen+1)},
		"long footer":   {FooterText: strings.Repeat("x", MaxFooterLen+1)},
		"control":       {HeaderText: "a\x07b"},
		"bidi override": {FooterText: "abc\u202edcba"},
		"bad utf8":      {HeaderText: "a\xffb"},
		"bad colour":    {Colors: Colors{Primary: "red"}},
		"short hex":     {Colors: Colors{Text: "#FFF"}},
		"unreadable":    {Colors: Colors{Text: "#777777"}},       // 4.48:1 on white
		"header bar":    {Colors: Colors{Primary: "#DDDDDD"}},    // white header text on light grey
		"faint accent":  {Colors: Colors{Accent: "#EEEEEE"}},     // near-invisible highlight
		"dark mode bad": {Colors: Colors{Background: "#000000"}}, // default dark text on black
	}
	for name, s := range cases {
		_, _, err := Check(s)
		var ve *ValidationError
		if !errors.As(err, &ve) || !errors.Is(err, ErrInvalid) || len(ve.Problems) == 0 {
			t.Errorf("%s: err = %v, want a *ValidationError", name, err)
		}
	}
	// A failing contrast still reports every ratio, for the editor.
	_, _, err := Check(Settings{Colors: Colors{Text: "#777777"}})
	var ve *ValidationError
	if errors.As(err, &ve) && len(ve.Contrasts) != 3 {
		t.Errorf("contrasts on failure = %+v", ve.Contrasts)
	}
}

func TestEffectiveAndRowRoundTrip(t *testing.T) {
	eff := Colors{Accent: "#123456"}.Effective()
	if eff.Accent != "#123456" || eff.Primary != Default.Primary || eff.Background != Default.Background {
		t.Fatalf("effective = %+v", eff)
	}
	s := Settings{HeaderText: "H", FooterText: "F", Colors: Colors{Primary: "#000001", Accent: "#000002", Background: "#FFFFFF", Text: "#000003"}}
	if got := FromRow(s.ToRow()); got != s {
		t.Fatalf("round trip = %+v", got)
	}
	_ = store.BrandingRow{}
}

func encode(t *testing.T, format string, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, 0, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&b, img)
	case "jpeg":
		err = jpeg.Encode(&b, img, nil)
	case "gif":
		err = gif.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestProcessLogoReencodesAndScales(t *testing.T) {
	for _, f := range []string{"png", "jpeg"} {
		logo, err := ProcessLogo(bytes.NewReader(encode(t, f, 1200, 1024)))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if logo.Height != LogoMaxHeight || logo.Width != 600 || logo.SHA256 == "" {
			t.Fatalf("%s: %dx%d", f, logo.Width, logo.Height)
		}
		if !bytes.HasPrefix(logo.PNG, []byte("\x89PNG")) {
			t.Fatalf("%s: stored logo is not PNG", f)
		}
	}
	small, err := ProcessLogo(bytes.NewReader(encode(t, "png", 64, 32)))
	if err != nil || small.Width != 64 || small.Height != 32 {
		t.Fatalf("small logo = %dx%d, %v (must not be upscaled)", small.Width, small.Height, err)
	}
	wide, _ := ProcessLogo(bytes.NewReader(encode(t, "png", 4000, 100)))
	if wide.Width != LogoMaxWidth {
		t.Fatalf("wide logo width = %d", wide.Width)
	}
}

func TestProcessLogoRejects(t *testing.T) {
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><circle r="5"/></svg>`)
	big := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxLogoUpload)...)
	cases := map[string][]byte{
		"svg":       svg,
		"gif":       encode(t, "gif", 10, 10),
		"empty":     {},
		"too large": big,
		"truncated": encode(t, "png", 50, 50)[:40],
		"too tall":  encode(t, "png", 10, MaxLogoSide+1),
	}
	for name, raw := range cases {
		if _, err := ProcessLogo(bytes.NewReader(raw)); !errors.Is(err, ErrBadLogo) {
			t.Errorf("%s: err = %v, want ErrBadLogo", name, err)
		}
	}
}

// A tiny file whose header claims enormous dimensions must be refused
// before it's decoded (a decompression bomb).
func TestProcessLogoRefusesBombBeforeDecoding(t *testing.T) {
	raw := encode(t, "png", 1, 1)
	// IHDR width and height are bytes 16..23: claim 4096×4096 (16.7M px,
	// within the per-side limit but over the pixel cap).
	putBE := func(b []byte, v uint32) { b[0], b[1], b[2], b[3] = byte(v>>24), byte(v>>16), byte(v>>8), byte(v) }
	putBE(raw[16:20], 4096)
	putBE(raw[20:24], 4096)
	putBE(raw[29:33], crc32.ChecksumIEEE(raw[12:29])) // keep the IHDR chunk valid
	if _, err := ProcessLogo(bytes.NewReader(raw)); !errors.Is(err, ErrBadLogo) || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want refused on dimensions", err)
	}
}

func FuzzProcessLogo(f *testing.F) {
	f.Add([]byte("\x89PNG\r\n\x1a\n"))
	f.Add([]byte("\xff\xd8\xff\xe0"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "))
	f.Add([]byte("<svg/>"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		logo, err := ProcessLogo(bytes.NewReader(raw))
		if err != nil {
			return
		}
		if logo.Width < 1 || logo.Height < 1 || logo.Height > LogoMaxHeight || logo.Width > LogoMaxWidth ||
			!bytes.HasPrefix(logo.PNG, []byte("\x89PNG")) {
			t.Fatalf("accepted logo out of bounds: %dx%d", logo.Width, logo.Height)
		}
	})
}
