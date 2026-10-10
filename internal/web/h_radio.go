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
	Report radiocheck.Report `json:"report"`
	Host   hostmon.Snapshot  `json:"host"`
}

// getRadio runs the radio status check (feedback 2026-10-09, item 6).
func (s *server) getRadio(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	var host hostmon.Snapshot
	if s.Host != nil {
		host = s.Host.Snapshot()
	}
	ctx, cancel := context.WithTimeout(r.Context(), radioCheckTimeout)
	defer cancel()
	writeJSON(w, http.StatusOK, radioView{Report: s.radio.Check(ctx, s.Graywolf, cfg, host), Host: host})
}
