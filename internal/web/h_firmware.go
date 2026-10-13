package web

import (
	"io"
	"net/http"
	"os"

	"checkin-board/internal/firmware"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

// Firmware for the UV-K5/UV-K1 plugged into this node (Station page,
// Radio card): files kept on the node, flashed by the node itself
// through the radio's bootloader, so the browser's connection doesn't
// matter once a flash has started.

// Flasher is the node's flash job (firmware.Flasher).
type Flasher interface {
	Start(name string) error
	Cancel() error
	Status() firmware.Status
}

type firmwareView struct {
	Files []firmware.Entry `json:"files"`
	Job   firmware.Status  `json:"job"`
}

type flashBody struct {
	// Confirm acknowledges that race reports wait while the radio is
	// being flashed (needed during a race).
	Confirm bool `json:"confirm"`
}

// firmwareReady answers 501 on a node without a UV-K5.
func (s *server) firmwareReady(w http.ResponseWriter, r *http.Request) bool {
	if s.Flasher == nil || s.firmware == nil {
		writeError(w, r, s.log, &httpError{http.StatusNotImplemented, "not_uvk5", "firmware flashing is for the UV-K5/UV-K1 radio type: choose it under Radio type first"})
		return false
	}
	return true
}

func (s *server) getFirmware(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	files, err := s.firmware.List()
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if files == nil {
		files = []firmware.Entry{}
	}
	writeJSON(w, http.StatusOK, firmwareView{Files: files, Job: s.Flasher.Status()})
}

func (s *server) postFirmware(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	f, err := uploadedFile(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	defer f.Close()
	upload := ""
	if fh := r.MultipartForm.File["file"]; len(fh) > 0 {
		upload = fh[0].Filename
	}
	data, err := io.ReadAll(io.LimitReader(f, uvk5.MaxFirmwareSize+1))
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	name := firmware.SafeName(upload)
	if name == "" {
		name = "firmware.bin"
	}
	if err := s.firmware.Save(name, data, r.FormValue("replace") == "1"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("web: firmware uploaded", "file", name, "bytes", len(data))
	writeJSON(w, http.StatusOK, map[string]any{"name": name, "size": len(data)})
}

func (s *server) deleteFirmware(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	if err := s.firmware.Delete(r.PathValue("name")); err != nil {
		if os.IsNotExist(err) {
			err = &httpError{http.StatusNotFound, "not_found", "no such firmware file on this node"}
		}
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("name")})
}

// postFlash starts flashing: the node takes the radio, waits for it in
// bootloader mode, and writes the file.
func (s *server) postFlash(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	var b flashBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.confirmDuringRace(r, b.Confirm, "flashing"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Flasher.Start(r.PathValue("name")); err != nil {
		if os.IsNotExist(err) {
			err = &httpError{http.StatusNotFound, "not_found", "no such firmware file on this node"}
		}
		writeError(w, r, s.log, err)
		return
	}
	s.log.Warn("web: firmware flash started", "file", r.PathValue("name"))
	writeJSON(w, http.StatusAccepted, s.Flasher.Status())
}

func (s *server) postFlashCancel(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	if err := s.Flasher.Cancel(); err != nil {
		writeError(w, r, s.log, &httpError{http.StatusConflict, "not_cancellable", err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.Flasher.Status())
}

// confirmDuringRace refuses to take the radio away during a race unless
// the operator confirmed.
func (s *server) confirmDuringRace(r *http.Request, confirmed bool, what string) error {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		return err
	}
	if cfg.RaceState == store.RaceActive && !confirmed {
		return &httpError{http.StatusConflict, "confirm_required",
			"the race is running: race reports wait while " + what + " has the radio; confirm to continue"}
	}
	return nil
}
