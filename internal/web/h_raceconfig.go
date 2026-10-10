package web

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"time"

	"checkin-board/internal/raceconfig"
	"checkin-board/internal/store"
)

// Race config files (phase 12a): a node loads its settings, HQ's
// checkpoint list, an event page and some graywolf settings from one
// file per race. Loading only sets values; everything stays editable.

// raceConfigTimeout bounds a preview or apply's graywolf calls.
const raceConfigTimeout = 15 * time.Second

type raceConfigList struct {
	Files []raceconfig.Entry `json:"files"`
	// CanApply: the race hasn't started (configs apply only then).
	CanApply bool `json:"can_apply"`
	// GraywolfBackup: graywolf settings saved before a config changed
	// them, for "Restore graywolf settings".
	GraywolfBackup bool `json:"graywolf_backup"`
}

type raceConfigStations struct {
	RaceName  string               `json:"race_name"`
	Stations  []raceconfig.Station `json:"stations"`
	Suggested string               `json:"suggested,omitempty"` // matches graywolf's callsign
}

type eventPageView struct {
	Text      string     `json:"text"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func (s *server) getRaceConfigs(w http.ResponseWriter, r *http.Request) {
	files, err := s.raceFolder.List()
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	backup, err := s.raceConfigs.HasGraywolfBackup(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if files == nil {
		files = []raceconfig.Entry{}
	}
	writeJSON(w, http.StatusOK, raceConfigList{Files: files, CanApply: cfg.RaceState == store.RaceSetup, GraywolfBackup: backup})
}

func (s *server) postRaceConfig(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(uploadMemory); err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "no_file", "upload a file in the \"file\" field"})
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "no_file", "upload a file in the \"file\" field"})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, raceconfig.MaxFileBytes+1))
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	name := raceconfig.SafeName(hdr.Filename)
	if name == "" {
		name = "race-config.json"
	}
	if err := s.raceFolder.Save(name, data, r.FormValue("replace") == "1"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("web: race config uploaded", "file", name)
	writeJSON(w, http.StatusOK, s.entry(name))
}

// entry is the folder's listing of one file.
func (s *server) entry(name string) raceconfig.Entry {
	files, _ := s.raceFolder.List()
	for _, e := range files {
		if e.Name == name {
			return e
		}
	}
	return raceconfig.Entry{Name: name}
}

func (s *server) deleteRaceConfig(w http.ResponseWriter, r *http.Request) {
	if err := s.raceFolder.Delete(r.PathValue("name")); err != nil {
		writeError(w, r, s.log, raceErr(err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("name")})
}

func (s *server) getRaceConfig(w http.ResponseWriter, r *http.Request) {
	f, err := s.raceFolder.Open(r.PathValue("name"))
	if err != nil {
		writeError(w, r, s.log, raceErr(err))
		return
	}
	out := raceConfigStations{RaceName: f.Race.Name, Stations: f.Stations()}
	ctx, cancel := context.WithTimeout(r.Context(), ownCallTimeout)
	defer cancel()
	if sc, err := s.Graywolf.StationConfig(ctx); err == nil {
		out.Suggested = f.StationFor(sc.Callsign)
	}
	writeJSON(w, http.StatusOK, out)
}

type raceConfigBody struct {
	Station  string `json:"station"`
	Graywolf bool   `json:"graywolf"` // apply the graywolf changes too
	Token    string `json:"token"`    // the preview's: apply exactly what was shown
}

// raceErr maps a missing file to 404 (only here: elsewhere a missing
// file is an internal problem).
func raceErr(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return &httpError{http.StatusNotFound, "not_found", "no such race config file on this node"}
	}
	return err
}

func (s *server) postRaceConfigPreview(w http.ResponseWriter, r *http.Request) {
	f, b, ok := s.raceConfigRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), raceConfigTimeout)
	defer cancel()
	p, err := s.raceConfigs.Preview(ctx, f, b.Station)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) postRaceConfigApply(w http.ResponseWriter, r *http.Request) {
	f, b, ok := s.raceConfigRequest(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), raceConfigTimeout)
	defer cancel()
	res, err := s.raceConfigs.Apply(ctx, f, b.Station, b.Graywolf, b.Token)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("web: race config applied", "file", r.PathValue("name"), "station", b.Station, "graywolf", b.Graywolf,
		"graywolf_errors", len(res.GraywolfErrors))
	writeJSON(w, http.StatusOK, res)
}

func (s *server) raceConfigRequest(w http.ResponseWriter, r *http.Request) (*raceconfig.File, raceConfigBody, bool) {
	var b raceConfigBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return nil, b, false
	}
	f, err := s.raceFolder.Open(r.PathValue("name"))
	if err != nil {
		writeError(w, r, s.log, raceErr(err))
		return nil, b, false
	}
	return f, b, true
}

func (s *server) getRaceConfigExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), raceConfigTimeout)
	defer cancel()
	data, err := s.raceConfigs.Export(ctx)
	if err != nil {
		cfg, gerr := s.Store.GetSettings(r.Context())
		if gerr == nil && cfg.Role != store.RoleHQ {
			writeError(w, r, s.log, &httpError{http.StatusConflict, "wrong_role", "export is for HQ: it holds the checkpoint list"})
			return
		}
		writeError(w, r, s.log, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="race-config.json"`)
	_, _ = w.Write(data)
}

func (s *server) postGraywolfRestore(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), raceConfigTimeout)
	defer cancel()
	notes, err := s.raceConfigs.RestoreGraywolf(ctx)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("web: graywolf settings restored")
	writeJSON(w, http.StatusOK, map[string]any{"restored": true, "notes": notes})
}

func (s *server) getEventPage(w http.ResponseWriter, r *http.Request) {
	text, at, err := s.Store.EventPage(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, eventPageView{Text: text, UpdatedAt: at})
}

func (s *server) putEventPage(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Text string `json:"text"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Store.SetEventPage(r.Context(), b.Text); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.getEventPage(w, r)
}
