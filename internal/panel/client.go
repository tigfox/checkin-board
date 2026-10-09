package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// Client is the app's local hook as a Source.
type Client struct {
	base, token string
	http        *http.Client
}

// NewClient talks to the app at base (e.g. http://127.0.0.1:8090).
func NewClient(base, token string) *Client {
	// A short timeout: the panel's loop (and its buttons) wait on it.
	return &Client{base: base, token: token, http: &http.Client{Timeout: 3 * time.Second}}
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrAppUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("app answered %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// View implements Source.
func (c *Client) View(ctx context.Context) (View, error) {
	var v View
	err := c.call(ctx, http.MethodGet, "/api/hook/panel", nil, &v)
	return v, err
}

// Act implements Source.
func (c *Client) Act(ctx context.Context, id int64, r ActionRequest) (ActionResult, error) {
	var out ActionResult
	err := c.call(ctx, http.MethodPost, "/api/hook/panel/actions/"+strconv.FormatInt(id, 10), r, &out)
	return out, err
}

// Refreshed implements Source.
func (c *Client) Refreshed(ctx context.Context, at time.Time) error {
	return c.call(ctx, http.MethodPost, "/api/hook/panel/refreshed", map[string]time.Time{"at": at}, nil)
}

// DetectGaveUp implements Source.
func (c *Client) DetectGaveUp(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/api/hook/panel/detect-gave-up", map[string]bool{"gave_up": true}, nil)
}

// SetController implements Source.
func (c *Client) SetController(ctx context.Context, controller string) error {
	return c.call(ctx, http.MethodPut, "/api/hook/panel/controller", map[string]string{"controller": controller}, nil)
}

// PNGDisplay is a stand-in display for trying the panel without the
// hardware: each refresh is written to dir as a numbered PNG.
type PNGDisplay struct {
	dir, controller string
	partial         bool
	mu              sync.Mutex
	frames          []string
}

// NewPNGDisplay writes frames to dir; partial says whether to act like a
// panel with partial refresh.
func NewPNGDisplay(dir, controller string, partial bool) *PNGDisplay {
	return &PNGDisplay{dir: dir, controller: controller, partial: partial}
}

func (d *PNGDisplay) save(kind string, img image.Image) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := filepath.Join(d.dir, fmt.Sprintf("%04d-%s-%s.png", len(d.frames)+1, kind, d.controller))
	f, err := os.Create(name)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	d.frames = append(d.frames, name)
	return nil
}

// Full implements epd.Display.
func (d *PNGDisplay) Full(img image.Image) error { return d.save("full", img) }

// Partial implements epd.Display.
func (d *PNGDisplay) Partial(img image.Image) error {
	if !d.partial {
		return fmt.Errorf("panel: %s has no partial refresh", d.controller)
	}
	return d.save("partial", img)
}

// CanPartial implements epd.Display.
func (d *PNGDisplay) CanPartial() bool { return d.partial }

// Close implements epd.Display.
func (d *PNGDisplay) Close() error { return nil }

// Frames lists the files written so far.
func (d *PNGDisplay) Frames() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.frames...)
}

// NodeAddrs lists the node's non-loopback IPv4 addresses (for the
// address volunteers type into their phones).
func NodeAddrs() []string {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}
