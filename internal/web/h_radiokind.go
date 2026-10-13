package web

import (
	"net/http"
	"strings"

	"checkin-board/internal/radiokind"
)

// The radio kind (Station page, Radio card): which radio this node
// sends race traffic through. It can change mid-race when a radio is
// swapped; the app restarts in a few seconds with the new kind, and
// reports wait in the outbox meanwhile.

type radioKindOption struct {
	radiokind.Kind
	Available bool   `json:"available"`
	Why       string `json:"why,omitempty"` // why it isn't available
}

type radioKindView struct {
	Current string            `json:"current"` // what the app runs with
	Chosen  string            `json:"chosen"`  // what was saved (differs until the restart)
	Kinds   []radioKindOption `json:"kinds"`
}

type radioKindBody struct {
	Kind string `json:"kind"`
}

// radioKindOptions lists every kind and whether this node can use it.
func (s *server) radioKindOptions() []radioKindOption {
	all := radiokind.All()
	out := make([]radioKindOption, len(all))
	for i, k := range all {
		out[i] = radioKindOption{Kind: k, Available: true}
		if k.NeedsGraywolf && !s.GraywolfReady && s.backend() != k.ID {
			out[i].Available, out[i].Why = false, "graywolf's login isn't set up on this node: run install.sh again (or set GW_USER and GW_PASSWORD_FILE in /etc/checkin-board/checkin-board.env)"
		}
	}
	return out
}

func (s *server) getRadioKind(w http.ResponseWriter, r *http.Request) {
	chosen, err := s.Store.RadioKind(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cur := s.backend()
	if chosen == "" {
		chosen = cur
	}
	writeJSON(w, http.StatusOK, radioKindView{Current: cur, Chosen: chosen, Kinds: s.radioKindOptions()})
}

// putRadioKind saves the kind and restarts the app with it.
func (s *server) putRadioKind(w http.ResponseWriter, r *http.Request) {
	var b radioKindBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	var opt *radioKindOption
	for _, o := range s.radioKindOptions() {
		if strings.EqualFold(o.ID, strings.TrimSpace(b.Kind)) {
			opt = &o
			break
		}
	}
	switch {
	case opt == nil:
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid_input",
			"radio kind must be one of " + strings.Join(radiokind.IDs(), ", ")})
		return
	case !opt.Available:
		writeError(w, r, s.log, &httpError{http.StatusConflict, "not_available", opt.Label + ": " + opt.Why})
		return
	}
	if err := s.Store.SetRadioKind(r.Context(), opt.ID); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	restarting := opt.ID != s.backend() && s.Restart != nil
	s.log.Info("web: radio kind chosen", "kind", opt.ID, "was", s.backend(), "restarting", restarting)
	if restarting {
		s.Restart()
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": opt.ID, "restarting": restarting})
}
