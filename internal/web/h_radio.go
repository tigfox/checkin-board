package web

import (
	"context"
	"net/http"
	"time"

	"checkin-board/internal/hostmon"
	"checkin-board/internal/radiocheck"
)

// radioCheckTimeout bounds the radio check's graywolf calls.
const radioCheckTimeout = 8 * time.Second

type radioView struct {
	Report  radiocheck.Report `json:"report"`
	Host    hostmon.Snapshot  `json:"host"`
	Backend string            `json:"backend"`
}

// getRadio runs the radio status check (feedback 2026-10-09, item 6).
func (s *server) getRadio(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	host := s.hostSnapshot()
	ctx, cancel := context.WithTimeout(r.Context(), radioCheckTimeout)
	defer cancel()
	var report radiocheck.Report
	if s.Radio != nil {
		report = s.radio.CheckRadio(ctx, s.Radio)
	} else {
		report = s.radio.Check(ctx, s.Graywolf, cfg, host)
	}
	writeJSON(w, http.StatusOK, radioView{Report: report, Host: host, Backend: s.backend()})
}
