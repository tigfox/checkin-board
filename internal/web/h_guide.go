package web

import (
	"embed"
	"net/http"
)

// The local configuration guide (feedback 2026-10-09, item 3). It lives
// outside static/ so it is served only behind the admin login: every
// page requires a login, and the guide is content, not a shell. It is
// entirely on the node: stations have no internet in the field
// (TestNoExternalURLs checks guideFS).
var (
	//go:embed guide/guide.html
	guidePage []byte
	//go:embed guide
	guideFS embed.FS
)

func (s *server) getGuide(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(guidePage)
}
