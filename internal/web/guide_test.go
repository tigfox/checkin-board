package web

import (
	"io"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestGuideServedToAdmin(t *testing.T) {
	e := newEnv(t, checkpointSettings("active"))
	resp := e.do("GET", "/guide", e.admin, nil)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /guide = %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"2 m band plan", "146.520", "144.390", "24 kHz", "callsign-SSID"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (login-only page)", resp.Header.Get("Cache-Control"))
	}
}

// offNode matches a URL that would reach off the node. Stations have no
// internet in the field, so the UI and guide must never depend on one.
// Allowed: the SVG namespace (an identifier, never fetched) and the
// node's own loopback, which the guide's graywolf commands use.
var offNode = regexp.MustCompile(`(?i)\b(https?|wss?)://[^\s"'<>)]+`)

func TestNoExternalURLs(t *testing.T) {
	allowed := func(u string) bool {
		for _, ok := range []string{"http://www.w3.org/2000/svg", "http://127.0.0.1", "http://localhost"} {
			if strings.HasPrefix(strings.ToLower(u), ok) {
				return true
			}
		}
		return false
	}
	for name, fsys := range map[string]fs.FS{"static": mustSub(t, staticFS, "static"), "guide": guideFS} {
		err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			b, err := fs.ReadFile(fsys, path)
			if err != nil {
				return err
			}
			for _, u := range offNode.FindAllString(string(b), -1) {
				if !allowed(u) {
					t.Errorf("%s/%s links off the node: %s", name, path, u)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func mustSub(t *testing.T, fsys fs.FS, dir string) fs.FS {
	t.Helper()
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		t.Fatal(err)
	}
	return sub
}
