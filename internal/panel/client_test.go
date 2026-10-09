package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/panel/menu"
)

func TestClientTalksToTheHook(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		got = append(got, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/hook/panel":
			_ = json.NewEncoder(w).Encode(View{Status: Status{Role: "checkpoint"}, Settings: Settings{Enabled: true}})
		case "/api/hook/panel/actions/7":
			var req ActionRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			_ = json.NewEncoder(w).Encode(ActionResult{OK: true, Message: "did " + string(req.Action) + " " + req.To})
		default:
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "tok")
	v, err := c.View(ctx)
	if err != nil || v.Status.Role != "checkpoint" {
		t.Fatalf("view = %+v, %v", v, err)
	}
	r, err := c.Act(ctx, 7, ActionRequest{Action: menu.LinkCheck, To: "N0CALL-1"})
	if err != nil || r.Message != "did link_check N0CALL-1" {
		t.Fatalf("act = %+v, %v", r, err)
	}
	if err := c.Refreshed(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := c.SetController(ctx, "ssd1680"); err != nil {
		t.Fatal(err)
	}
	want := "GET /api/hook/panel|POST /api/hook/panel/actions/7|POST /api/hook/panel/refreshed|PUT /api/hook/panel/controller"
	if strings.Join(got, "|") != want {
		t.Fatalf("calls = %v", got)
	}
	if _, err := NewClient(srv.URL, "wrong").View(ctx); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
	if _, err := NewClient("http://127.0.0.1:1", "tok").View(ctx); err == nil {
		t.Fatal("no server: no error")
	}
}

func TestPNGDisplayWritesFrames(t *testing.T) {
	dir := t.TempDir()
	d := NewPNGDisplay(dir, "ssd1680z", true)
	if err := d.Full(TestPattern("x")); err != nil {
		t.Fatal(err)
	}
	if err := d.Partial(MenuScreen("Menu", []string{"a"}, 0)); err != nil {
		t.Fatal(err)
	}
	if !d.CanPartial() || d.Close() != nil {
		t.Fatal("caps")
	}
	n := NewPNGDisplay(dir, "ssd1675", false)
	if n.CanPartial() || n.Partial(TestPattern("x")) == nil {
		t.Fatal("a panel without partial refresh accepted one")
	}
	files := d.Frames()
	if len(files) != 2 || !strings.Contains(files[0], "full-ssd1680z") || !strings.Contains(files[1], "partial") {
		t.Fatalf("frames = %v", files)
	}
}

func TestNodeAddrs(t *testing.T) {
	for _, a := range NodeAddrs() {
		if strings.HasPrefix(a, "127.") || strings.Contains(a, ":") {
			t.Errorf("unexpected address %q (want non-loopback IPv4)", a)
		}
	}
}
