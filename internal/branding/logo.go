package branding

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"

	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// Logo limits (spec 8.3).
const (
	MaxLogoUpload = 1 << 20    // bytes
	MaxLogoSide   = 4096       // pixels, either side, before scaling
	MaxLogoPixels = 12_000_000 // below MaxLogoSide², so a max-size square is refused before decoding
	LogoMaxHeight = 512        // stored height cap
	LogoMaxWidth  = 2048
)

// ErrBadLogo wraps every logo rejection (HTTP 400).
var ErrBadLogo = errors.New("branding: logo rejected")

// Logo is a processed logo: always PNG, never the uploaded bytes.
type Logo struct {
	PNG    []byte
	Width  int
	Height int
	SHA256 string
}

// decoders are the only accepted formats. SVG is deliberately absent:
// it can carry script.
var decoders = map[string]struct {
	config func(io.Reader) (image.Config, error)
	decode func(io.Reader) (image.Image, error)
}{
	"png":  {png.DecodeConfig, png.Decode},
	"jpeg": {jpeg.DecodeConfig, jpeg.Decode},
	"webp": {webp.DecodeConfig, webp.Decode},
}

// ProcessLogo reads an uploaded image (at most MaxLogoUpload bytes),
// checks its format and dimensions before decoding it fully (so a small
// file claiming huge dimensions can't exhaust memory), scales it to fit
// LogoMaxHeight × LogoMaxWidth, and re-encodes it as PNG.
func ProcessLogo(r io.Reader) (Logo, error) {
	raw, err := io.ReadAll(io.LimitReader(r, MaxLogoUpload+1))
	if err != nil {
		return Logo{}, fmt.Errorf("branding: read logo: %w", err)
	}
	if len(raw) > MaxLogoUpload {
		return Logo{}, fmt.Errorf("%w: larger than %d bytes", ErrBadLogo, MaxLogoUpload)
	}
	format := sniff(raw)
	dec, ok := decoders[format]
	if !ok {
		return Logo{}, fmt.Errorf("%w: use a PNG, JPEG or WebP image (SVG and other formats are not accepted)", ErrBadLogo)
	}
	cfg, err := dec.config(bytes.NewReader(raw))
	if err != nil {
		return Logo{}, fmt.Errorf("%w: unreadable %s: %v", ErrBadLogo, format, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > MaxLogoSide || cfg.Height > MaxLogoSide ||
		cfg.Width*cfg.Height > MaxLogoPixels {
		return Logo{}, fmt.Errorf("%w: %d×%d is outside 1..%d px per side", ErrBadLogo, cfg.Width, cfg.Height, MaxLogoSide)
	}
	img, err := dec.decode(bytes.NewReader(raw))
	if err != nil {
		return Logo{}, fmt.Errorf("%w: unreadable %s: %v", ErrBadLogo, format, err)
	}
	scaled := scale(img)
	var out bytes.Buffer
	if err := png.Encode(&out, scaled); err != nil {
		return Logo{}, fmt.Errorf("branding: encode logo: %w", err)
	}
	sum := sha256.Sum256(out.Bytes())
	b := scaled.Bounds()
	return Logo{PNG: out.Bytes(), Width: b.Dx(), Height: b.Dy(), SHA256: hex.EncodeToString(sum[:])}, nil
}

// sniff identifies the format from magic bytes (never from the file
// name or the client's Content-Type).
func sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return "jpeg"
	case len(b) >= 12 && bytes.Equal(b[0:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	default:
		return ""
	}
}

// scale fits img within LogoMaxWidth × LogoMaxHeight, keeping its
// aspect ratio; it always returns a fresh RGBA image (stripping any
// metadata or odd colour model of the original).
func scale(img image.Image) *image.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	f := min(1, float64(LogoMaxHeight)/float64(h), float64(LogoMaxWidth)/float64(w))
	nw, nh := max(1, int(float64(w)*f+0.5)), max(1, int(float64(h)*f+0.5))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}
