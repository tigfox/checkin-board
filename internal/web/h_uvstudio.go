package web

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// UV Studio (Armel FAUVEAU, Apache-2.0; internal/web/uvstudio/VENDORED.md)
// runs in the browser and talks to a radio through Web Serial, which only
// reaches the browser's own USB ports. On a UV-K5 node the radio is on
// the Pi, so uvstudio/cb/serial-bridge.js stands in for Web Serial and
// carries the bytes over a WebSocket to this handler, which has the
// radio's port while the page uses it. Race reports wait meanwhile.

//go:embed all:uvstudio
var uvstudioEmbed embed.FS

var uvstudioFS = mustSubFS(uvstudioEmbed, "uvstudio")

func mustSubFS(f fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// uvstudioIdle gives the radio back when the page has seen no one use
// it for this long (the page reports clicks and keys).
const uvstudioIdle = 10 * time.Minute

// RadioBridge lends the UV-K5's serial port to UV Studio.
type RadioBridge interface {
	// Open takes the radio from the backend and opens its port, DTR up.
	// Closing the port gives the radio back.
	Open(ctx context.Context) (BridgePort, error)
}

// BridgePort is the radio's open port.
type BridgePort interface {
	io.ReadWriteCloser
	Name() string
}

// bridgeMsg is a control message (text frame); bytes travel as binary.
type bridgeMsg struct {
	Type    string `json:"type"` // ready, error, idle, closed (server); active (page)
	Port    string `json:"port,omitempty"`
	Message string `json:"message,omitempty"`
}

// getUVStudio serves the UV Studio page and its files.
func (s *server) getUVStudio(w http.ResponseWriter, r *http.Request) {
	if s.RadioBridge == nil {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>UV Studio</title>`+
			`<p>UV Studio is for the UV-K5/UV-K1 radio type. Choose it on <a href="/admin.html">Admin → Station → Radio</a> first.</p>`)
		return
	}
	p := path.Clean("/" + r.PathValue("path"))[1:]
	if p == "" || p == "." {
		p = "index.html"
	}
	data, err := fs.ReadFile(uvstudioFS, p)
	if err != nil || strings.HasPrefix(p, ".") {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(p))
	if ct == "" {
		ct = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Cache-Control", "no-cache")
	// UV Studio draws previews from data: and blob: images; everything
	// else stays this node's own (nothing reaches the internet).
	h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: blob:; style-src 'self'; script-src 'self'; "+
		"connect-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	_, _ = w.Write(data)
}

// getUVStudioWS is the serial bridge.
func (s *server) getUVStudioWS(w http.ResponseWriter, r *http.Request) {
	if s.RadioBridge == nil {
		writeError(w, r, s.log, &httpError{http.StatusNotImplemented, "not_uvk5", "UV Studio is for the UV-K5/UV-K1 radio type"})
		return
	}
	// GET skips the same-site check: a WebSocket must come from our page.
	if u, err := url.Parse(r.Header.Get("Origin")); err != nil || u.Host != r.Host {
		writeError(w, r, s.log, &httpError{http.StatusForbidden, "forbidden", "cross-origin request refused"})
		return
	}
	if err := s.confirmDuringRace(r, r.URL.Query().Get("confirm") == "1", "UV Studio"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()
	port, perr := s.RadioBridge.Open(ctx)
	conn, brw, _, err := ws.UpgradeHTTP(r, w)
	if err != nil {
		if perr == nil {
			_ = port.Close()
		}
		s.log.Warn("web: uvstudio upgrade", "err", err)
		return
	}
	_ = conn.SetDeadline(time.Time{}) // the server's read/write timeouts don't apply
	b := &bridge{conn: conn, in: brw.Reader, log: s}
	defer conn.Close()
	if perr != nil {
		b.control(bridgeMsg{Type: "error", Message: safeBridgeErr(perr)})
		return
	}
	s.log.Info("web: uvstudio has the radio", "port", port.Name())
	b.control(bridgeMsg{Type: "ready", Port: port.Name()})
	reason := b.run(ctx, port)
	_ = port.Close()
	s.log.Info("web: uvstudio gave the radio back", "why", reason)
	b.control(bridgeMsg{Type: "closed", Message: reason})
}

func safeBridgeErr(err error) string {
	return "can't open the radio: " + err.Error()
}

type bridge struct {
	conn net.Conn
	in   io.Reader
	log  *server
	wmu  sync.Mutex
}

func (b *bridge) write(op ws.OpCode, p []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return wsutil.WriteServerMessage(b.conn, op, p)
}

func (b *bridge) control(m bridgeMsg) {
	data, _ := json.Marshal(m)
	_ = b.write(ws.OpText, data)
}

// lockedRW lets wsutil answer pings while the pump writes.
type lockedRW struct {
	io.Reader
	b *bridge
}

func (l lockedRW) Write(p []byte) (int, error) {
	l.b.wmu.Lock()
	defer l.b.wmu.Unlock()
	return l.b.conn.Write(p)
}

// run pumps bytes both ways until either side ends or the page is idle.
// It returns why it ended.
func (b *bridge) run(ctx context.Context, port BridgePort) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ended := make(chan string, 3)
	go func() { // radio -> page
		buf := make([]byte, 4096)
		for {
			n, err := port.Read(buf)
			if n > 0 {
				if b.write(ws.OpBinary, buf[:n]) != nil {
					ended <- "page gone"
					return
				}
			}
			if err != nil {
				ended <- "radio port closed (unplugged or restarted?)"
				return
			}
		}
	}()
	active := make(chan struct{}, 1)
	go func() { // page -> radio
		rw := lockedRW{b.in, b}
		for {
			data, op, err := wsutil.ReadClientData(rw)
			if err != nil {
				ended <- "page closed"
				return
			}
			switch op {
			case ws.OpBinary:
				if _, err := port.Write(data); err != nil {
					ended <- "radio port closed (unplugged or restarted?)"
					return
				}
			case ws.OpText:
				var m bridgeMsg
				if json.Unmarshal(data, &m) == nil && m.Type == "active" {
					select {
					case active <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	idle := time.NewTimer(uvstudioIdle)
	defer idle.Stop()
	for {
		select {
		case why := <-ended:
			return why
		case <-active:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(uvstudioIdle)
		case <-idle.C:
			b.control(bridgeMsg{Type: "idle", Message: "UV Studio was idle for 10 minutes: the radio is back with checkin-board"})
			return "idle"
		case <-ctx.Done():
			return "shutting down"
		}
	}
}
