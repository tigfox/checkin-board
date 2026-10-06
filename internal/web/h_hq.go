package web

import (
	"bytes"
	"checkin-board/internal/linkcheck"
	"net/http"
	"strconv"
	"strings"

	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

func (s *server) getBoard(w http.ResponseWriter, r *http.Request) {
	b, err := s.Ops.Board(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, b)
}

// getStatus is HQ's checkpoint health panel plus recent bad reports.
func (s *server) getStatus(w http.ResponseWriter, r *http.Request) {
	if err := s.requireRole(r, store.RoleHQ); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	health, err := s.HQ.Health(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	bad, err := s.Store.ListBadReports(r.Context(), 50)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cps, checks, responses, err := s.linkInputs(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": health, "bad_reports": bad,
		"links": linkcheck.Latest(cps, checks, responses)})
}

func (s *server) postRerequest(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if cfg.Role != store.RoleHQ {
		writeError(w, r, s.log, &httpError{http.StatusConflict, "wrong_role", "not available in this node's role"})
		return
	}
	n, err := s.HQ.Rearm(r.Context(), cfg, strings.ToUpper(r.PathValue("cp")))
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"requested": n})
}

func (s *server) getResultsExport(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if err := s.Ops.ExportResults(r.Context(), &buf); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	csvDownload(w, "results.csv", buf.Bytes())
}

// requireRole fails unless this node has the given race role.
func (s *server) requireRole(r *http.Request, role string) error {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		return err
	}
	if cfg.Role != role {
		return &httpError{http.StatusConflict, "wrong_role", "not available in this node's role"}
	}
	return nil
}

type checkpointBody struct {
	Code         string `json:"code"`
	Name         string `json:"name"`
	CourseOrder  int    `json:"course_order"`
	ExpectedCall string `json:"expected_call"`
}

func (b checkpointBody) model() store.Checkpoint {
	return store.Checkpoint{Code: strings.ToUpper(strings.TrimSpace(b.Code)), Name: strings.TrimSpace(b.Name),
		CourseOrder: b.CourseOrder, ExpectedCall: strings.ToUpper(strings.TrimSpace(b.ExpectedCall))}
}

func (s *server) getCheckpoints(w http.ResponseWriter, r *http.Request) {
	cps, err := s.Store.ListCheckpoints(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": cps})
}

func (s *server) postCheckpoint(w http.ResponseWriter, r *http.Request) {
	var b checkpointBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	c := b.model()
	if err := s.Store.CreateCheckpoint(r.Context(), &c); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

func pathID(r *http.Request) (uint, error) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil || id == 0 {
		return 0, &httpError{http.StatusBadRequest, "invalid", "bad id"}
	}
	return uint(id), nil
}

func (s *server) putCheckpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	var b checkpointBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	c := b.model()
	c.ID = id
	if err := s.Store.UpdateCheckpoint(r.Context(), &c); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *server) deleteCheckpoint(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Store.DeleteCheckpoint(r.Context(), id); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getRunners(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.ListRunners(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runners": rs})
}

type runnerBody struct {
	Bib      string `json:"bib"`
	Category string `json:"category"`
}

func (s *server) postRunner(w http.ResponseWriter, r *http.Request) {
	var b runnerBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	bib, err := wire.ParseBib(b.Bib)
	if err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid_bib", "bib must be 1-9999"})
		return
	}
	if _, err := s.Store.UpsertRunners(r.Context(), []store.Runner{{Bib: bib, Category: strings.TrimSpace(b.Category)}}); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func pathBib(r *http.Request) (store.Bib, error) {
	bib, err := wire.ParseBib(r.PathValue("bib"))
	if err != nil {
		return 0, &httpError{http.StatusBadRequest, "invalid_bib", "bib must be 1-9999"}
	}
	return bib, nil
}

func (s *server) deleteRunner(w http.ResponseWriter, r *http.Request) {
	bib, err := pathBib(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Store.DeleteRunner(r.Context(), bib); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) getRunnerHistory(w http.ResponseWriter, r *http.Request) {
	bib, err := pathBib(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	h, err := s.Ops.RunnerHistory(r.Context(), bib)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

// postRunnerImport loads a roster CSV (bib, and category only from a
// column headed "category"; everything else is discarded: spec 4.2.5).
func (s *server) postRunnerImport(w http.ResponseWriter, r *http.Request) {
	f, err := uploadedFile(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	defer f.Close()
	runners, err := store.ParseRosterCSV(f)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	n, err := s.Store.UpsertRunners(r.Context(), runners)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"runners": n})
}

func (s *server) postRecoveryImport(w http.ResponseWriter, r *http.Request) {
	f, err := uploadedFile(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	defer f.Close()
	res, err := s.Ops.ImportCheckpointExport(r.Context(), f)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *server) postJournalImport(w http.ResponseWriter, r *http.Request) {
	f, err := uploadedFile(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	defer f.Close()
	res, err := s.Ops.ImportJournal(r.Context(), f)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
