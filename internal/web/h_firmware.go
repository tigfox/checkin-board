package web

import (
	"errors"
	"io"
	"net/http"
	"os"

	"checkin-board/internal/firmware"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
)

// Firmware slots of the UV-K5/UV-K1 plugged into this node (Station
// page): firmware files kept on the node, written by the node itself into
// the radio's slots 1-4 (external flash; the APRS and FM/voice firmware),
// with the radio in normal mode. The radio's main firmware is never
// written. The browser's connection doesn't matter once a write starts.

// SlotWriter is the node's slot job (firmware.Slots).
type SlotWriter interface {
	ReadSlots() error
	Write(file string, slot int, name, version string, replaceMain bool) error
	Cancel() error
	Status() firmware.Status
}

type firmwareView struct {
	Files []firmware.Entry `json:"files"`
	Job   firmware.Status  `json:"job"`
}

type slotReadBody struct {
	// Confirm acknowledges that race reports wait while the radio is
	// busy (needed during a race).
	Confirm bool `json:"confirm"`
}

type slotWriteBody struct {
	Slot    int    `json:"slot"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Confirm bool   `json:"confirm"`
	// ReplaceMain allows overwriting the slot that holds the radio's main
	// firmware (Fusion).
	ReplaceMain bool `json:"replace_main"`
}

// firmwareReady answers 501 on a node without a UV-K5.
func (s *server) firmwareReady(w http.ResponseWriter, r *http.Request) bool {
	if s.Slots == nil || s.firmware == nil {
		writeError(w, r, s.log, &httpError{http.StatusNotImplemented, "not_uvk5", "firmware slots are for the UV-K5/UV-K1 radio type: choose it under Radio type first"})
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
	writeJSON(w, http.StatusOK, firmwareView{Files: files, Job: s.Slots.Status()})
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
	data, err := io.ReadAll(io.LimitReader(f, uvk5.MaxSlotImage+1))
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

// postSlotsRead reads what the radio's slots hold.
func (s *server) postSlotsRead(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	var b slotReadBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.confirmDuringRace(r, b.Confirm, "reading the firmware slots"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Slots.ReadSlots(); err != nil {
		writeError(w, r, s.log, slotErr(err))
		return
	}
	writeJSON(w, http.StatusOK, s.Slots.Status())
}

// postSlotWrite starts writing a firmware file into a slot.
func (s *server) postSlotWrite(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	var b slotWriteBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.confirmDuringRace(r, b.Confirm, "writing a firmware slot"); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Slots.Write(r.PathValue("name"), b.Slot, b.Name, b.Version, b.ReplaceMain); err != nil {
		if os.IsNotExist(err) {
			err = &httpError{http.StatusNotFound, "not_found", "no such firmware file on this node"}
		}
		writeError(w, r, s.log, err)
		return
	}
	s.log.Warn("web: firmware slot write started", "file", r.PathValue("name"), "slot", b.Slot, "name", b.Name)
	writeJSON(w, http.StatusAccepted, s.Slots.Status())
}

func (s *server) postSlotCancel(w http.ResponseWriter, r *http.Request) {
	if !s.firmwareReady(w, r) {
		return
	}
	if err := s.Slots.Cancel(); err != nil {
		writeError(w, r, s.log, &httpError{http.StatusConflict, "not_cancellable", err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.Slots.Status())
}

// slotErr says plainly that the radio can't do slots, or isn't there.
func slotErr(err error) error {
	if errors.Is(err, uvk5.ErrNoSlots) {
		return &httpError{http.StatusConflict, "no_slots", "the radio's firmware has no firmware slots (it needs an F4HWN firmware with multiboot)"}
	}
	var he *httpError
	if errors.As(err, &he) || errors.Is(err, firmware.ErrBusy) || errors.Is(err, radiobackend.ErrLent) {
		return err
	}
	return &httpError{http.StatusBadGateway, "radio_error", err.Error()}
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
