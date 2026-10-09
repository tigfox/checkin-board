package main

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/config"
	"checkin-board/internal/panel"
	"checkin-board/internal/panel/menu"
)

func TestPanelCommandDryRun(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/hook/panel" {
			_ = json.NewEncoder(w).Encode(panel.View{
				Status:   panel.Status{Role: "checkpoint", StateLabel: "Checkpoint open", Station: "AID3", CPCode: "AS5", GraywolfOK: true, Port: 8090, Now: time.Now()},
				Settings: panel.Settings{Enabled: true, Controller: "ssd1680z", RefreshMin: 5},
				Menu:     []panel.MenuItem{{Item: menu.Item{ID: 1, Label: "Status", Action: menu.Status, Roles: "both", Enabled: true}, Available: true}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer app.Close()
	tokFile := filepath.Join(t.TempDir(), "tok")
	_ = os.WriteFile(tokFile, []byte(strings.Repeat("k", config.MinHookTokenLen)), 0o600)
	frames := t.TempDir()
	env := config.Env{
		Getenv: func(k string) string {
			return map[string]string{config.EnvHookTokenFile: tokFile, config.EnvListen: strings.TrimPrefix(app.URL, "http://")}[k]
		},
		Open: func(name string) (fs.File, error) { return os.Open(name) },
	}
	pr, pw := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte("t\n"))
	}()
	err := runPanel(ctx, env, []string{"-display", "png:" + frames}, pr, slog.New(slog.DiscardHandler))
	if err != nil && err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(frames, "*.png"))
	if len(files) < 2 || !strings.Contains(files[0], "full") || !strings.Contains(files[1], "partial") {
		t.Fatalf("frames = %v (want a full status, then a partial menu)", files)
	}
}

func TestPanelCommandArgs(t *testing.T) {
	env := config.Env{Getenv: func(string) string { return "" }}
	if err := runPanel(context.Background(), env, []string{"-display", "png:/tmp"}, strings.NewReader(""), slog.New(slog.DiscardHandler)); err == nil || !strings.Contains(err.Error(), config.EnvHookTokenFile) {
		t.Fatalf("no token: %v", err)
	}
	for _, bad := range []string{"epd", "lcd", "png:"} {
		if _, err := displayOpener(bad); err == nil {
			t.Errorf("display %q accepted", bad)
		}
	}
	if b, ok := parseButton("top"); !ok || b != panel.Top {
		t.Error("top")
	}
	if b, ok := parseButton("B"); !ok || b != panel.Bottom {
		t.Error("bottom")
	}
	if _, ok := parseButton("x"); ok {
		t.Error("x")
	}
}
