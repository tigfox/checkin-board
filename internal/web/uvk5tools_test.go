package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"

	"checkin-board/internal/firmware"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

type fakeSlots struct {
	mu      sync.Mutex
	writes  []string
	st      firmware.Status
	readErr error
}

func (f *fakeSlots) ReadSlots() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return f.readErr
	}
	f.st.State, f.st.Slots = firmware.StateDone, []uvk5.SlotInfo{{Slot: 1, State: "valid", Name: "Aprs", Version: "v6.1", Size: 90000}, {Slot: 2, State: "empty"}}
	return nil
}

func (f *fakeSlots) Write(file string, slot int, name, version string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.State == firmware.StateWriting {
		return firmware.ErrBusy
	}
	if slot < 1 || slot > 4 {
		return firmware.ErrInvalid
	}
	f.writes = append(f.writes, fmt.Sprintf("%s->%d %s %s", file, slot, name, version))
	f.st = firmware.Status{State: firmware.StateWriting, Step: "writing", File: file, Slot: slot, Name: name, Done: 512, Total: 2048}
	return nil
}

func (f *fakeSlots) Cancel() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.st.State != firmware.StateWriting {
		return errors.New("nothing to cancel")
	}
	f.st.State, f.st.Error = firmware.StateFailed, "cancelled"
	return nil
}

func (f *fakeSlots) Status() firmware.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st
}

func uploadFirmware(t *testing.T, e *env, name string, data []byte) *http.Response {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", name)
	_, _ = fw.Write(data)
	_ = mw.Close()
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/admin/firmware", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: e.admin})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestFirmwareEndpoints(t *testing.T) {
	sl := &fakeSlots{st: firmware.Status{State: firmware.StateIdle}}
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) {
		d.Radio, d.RadioKind, d.Slots, d.FirmwareDir = connectedRadio(), "uvk5", sl, t.TempDir()
	})
	resp := uploadFirmware(t, e, "f4hwn.aprs.v6.1.bin", []byte("image"))
	if out := decode[map[string]any](t, resp); out["name"] != "f4hwn.aprs.v6.1.bin" {
		t.Fatalf("upload = %v", out)
	}
	expect(t, uploadFirmware(t, e, "f4hwn.aprs.v6.1.bin", []byte("image")), http.StatusConflict)
	expect(t, uploadFirmware(t, e, "empty.bin", nil), http.StatusBadRequest)
	v := decode[firmwareView](t, e.do("GET", "/api/admin/firmware", e.admin, nil))
	if len(v.Files) != 1 || v.Files[0].SlotName != "Aprs" || v.Files[0].SlotVersion != "v6.1" || v.Job.State != "idle" {
		t.Fatalf("view = %+v", v)
	}
	// During a race, the radio is only taken when confirmed.
	expect(t, e.do("POST", "/api/admin/firmware-slots/read", e.admin, slotReadBody{}), http.StatusConflict)
	if st := decode[firmware.Status](t, e.do("POST", "/api/admin/firmware-slots/read", e.admin, slotReadBody{Confirm: true})); len(st.Slots) != 2 || st.Slots[0].Name != "Aprs" {
		t.Fatalf("read = %+v", st)
	}
	write := slotWriteBody{Slot: 2, Name: "Aprs", Version: "v6.1"}
	expect(t, e.do("POST", "/api/admin/firmware/f4hwn.aprs.v6.1.bin/write", e.admin, write), http.StatusConflict)
	write.Confirm = true
	if st := decode[firmware.Status](t, e.do("POST", "/api/admin/firmware/f4hwn.aprs.v6.1.bin/write", e.admin, write)); st.State != "writing" || st.Slot != 2 {
		t.Fatalf("write = %+v", st)
	}
	if len(sl.writes) != 1 || sl.writes[0] != "f4hwn.aprs.v6.1.bin->2 Aprs v6.1" {
		t.Fatalf("writes = %v", sl.writes)
	}
	expect(t, e.do("POST", "/api/admin/firmware/f4hwn.aprs.v6.1.bin/write", e.admin, write), http.StatusConflict)
	if st := decode[firmware.Status](t, e.do("POST", "/api/admin/firmware-slots/cancel", e.admin, nil)); st.Error != "cancelled" {
		t.Fatalf("cancel = %+v", st)
	}
	write.Slot = 0
	expect(t, e.do("POST", "/api/admin/firmware/f4hwn.aprs.v6.1.bin/write", e.admin, write), http.StatusBadRequest)
	// A radio without slots is said plainly.
	sl.readErr = uvk5.ErrNoSlots
	if r := e.do("POST", "/api/admin/firmware-slots/read", e.admin, slotReadBody{Confirm: true}); r.StatusCode != http.StatusConflict {
		t.Fatalf("no slots: %d", r.StatusCode)
	}
	expect(t, e.do("DELETE", "/api/admin/firmware/f4hwn.aprs.v6.1.bin", e.admin, nil), http.StatusOK)
	expect(t, e.do("DELETE", "/api/admin/firmware/f4hwn.aprs.v6.1.bin", e.admin, nil), http.StatusNotFound)
	expect(t, e.do("GET", "/api/admin/firmware", e.volunt, nil), http.StatusForbidden)
}

// A graywolf node has no firmware tools or UV Studio.
func TestUVK5ToolsOnlyOnUVK5(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	expect(t, e.do("GET", "/api/admin/firmware", e.admin, nil), http.StatusNotImplemented)
	expect(t, e.do("GET", "/uvstudio/", e.admin, nil), http.StatusConflict)
}

// fakeBridge hands out one end of a pipe as the radio's port.
type fakeBridge struct {
	mu     sync.Mutex
	radio  net.Conn // the "radio" end
	closed chan struct{}
	err    error
	opens  int
}

type pipeBridgePort struct {
	net.Conn
	closed chan struct{}
	once   sync.Once
}

func (p *pipeBridgePort) Name() string { return "/dev/ttyACM0" }
func (p *pipeBridgePort) Close() error {
	p.once.Do(func() { close(p.closed) })
	return p.Conn.Close()
}

func (b *fakeBridge) Open(context.Context) (BridgePort, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	host, radio := net.Pipe()
	b.radio, b.closed = radio, make(chan struct{})
	b.opens++
	return &pipeBridgePort{Conn: host, closed: b.closed}, nil
}

func dialBridge(t *testing.T, e *env, query string, origin string) (net.Conn, error) {
	t.Helper()
	u := strings.Replace(e.srv.URL, "http://", "ws://", 1) + "/api/admin/uvstudio/ws" + query
	h := http.Header{}
	h.Set("Cookie", sessionCookie+"="+e.admin)
	h.Set("Origin", origin)
	conn, br, _, err := ws.Dialer{Header: ws.HandshakeHeaderHTTP(h)}.Dial(context.Background(), u)
	if err != nil || br == nil {
		return conn, err
	}
	return bufferedConn{conn, br}, nil // the server's first frame may be in br
}

type bufferedConn struct {
	net.Conn
	r io.Reader
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func readControl(t *testing.T, conn net.Conn) bridgeMsg {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, op, err := wsutil.ReadServerData(conn)
	if err != nil || op != ws.OpText {
		t.Fatalf("control: %v %v", op, err)
	}
	var m bridgeMsg
	_ = json.Unmarshal(data, &m)
	return m
}

func TestUVStudioBridge(t *testing.T) {
	br := &fakeBridge{}
	e := newEnvWith(t, checkpointSettings(store.RaceActive), func(d *Deps) {
		d.Radio, d.RadioKind, d.RadioBridge = connectedRadio(), "uvk5", br
	})
	origin := e.srv.URL
	// The page, with a policy that keeps it on the node.
	resp := e.do("GET", "/uvstudio/", e.admin, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "cb/serial-bridge.js") || strings.Contains(string(body), "flash-counter.js") ||
		!strings.Contains(resp.Header.Get("Content-Security-Policy"), "connect-src 'self'") {
		t.Fatalf("page = %d %s", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	expect(t, e.do("GET", "/uvstudio/js/k5viewer.js", e.admin, nil), http.StatusOK)
	expect(t, e.do("GET", "/uvstudio/js/studio-flash-counter.js", e.admin, nil), http.StatusNotFound)
	expect(t, e.do("GET", "/uvstudio/", e.volunt, nil), http.StatusForbidden)

	// Cross-origin, or unconfirmed during a race: refused.
	if _, err := dialBridge(t, e, "?confirm=1", "http://evil.example"); err == nil {
		t.Fatal("cross-origin bridge accepted")
	}
	if _, err := dialBridge(t, e, "", origin); err == nil {
		t.Fatal("bridge during a race without confirming")
	}
	conn, err := dialBridge(t, e, "?confirm=1", origin)
	if err != nil {
		t.Fatal(err)
	}
	if m := readControl(t, conn); m.Type != "ready" || m.Port != "/dev/ttyACM0" {
		t.Fatalf("ready = %+v", m)
	}
	// Page -> radio.
	if err := wsutil.WriteClientMessage(conn, ws.OpBinary, []byte{0x55, 0xAA, 0, 0}); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 4)
	_ = br.radio.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(br.radio, got); err != nil || !bytes.Equal(got, []byte{0x55, 0xAA, 0, 0}) {
		t.Fatalf("radio got % x %v", got, err)
	}
	// Radio -> page.
	go func() { _, _ = br.radio.Write([]byte{0xAA, 0x55, 1, 2}) }()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	data, op, err := wsutil.ReadServerData(conn)
	if err != nil || op != ws.OpBinary || !bytes.Equal(data, []byte{0xAA, 0x55, 1, 2}) {
		t.Fatalf("page got % x %v %v", data, op, err)
	}
	// Activity is accepted; closing the page gives the radio back.
	_ = wsutil.WriteClientMessage(conn, ws.OpText, []byte(`{"type":"active"}`))
	_ = conn.Close()
	select {
	case <-br.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the radio wasn't given back")
	}

	// The radio already lent: the page is told why.
	br.err = radiobackend.ErrLent
	conn, err = dialBridge(t, e, "?confirm=1", origin)
	if err != nil {
		t.Fatal(err)
	}
	if m := readControl(t, conn); m.Type != "error" || !strings.Contains(m.Message, "in use") {
		t.Fatalf("lent = %+v", m)
	}
	_ = conn.Close()
}
