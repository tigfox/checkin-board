package web

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"time"

	"checkin-board/internal/ops"
	"checkin-board/internal/store"
)

// testedGraywolfVersion is the graywolf release this build was
// contract-tested against.
const testedGraywolfVersion = "0.14.14"

// settingsBody is the editable settings (lifecycle fields are changed
// only by the race actions). Codes, calls and the path are upper-cased.
type settingsBody struct {
	Role            string   `json:"role"`
	RaceName        string   `json:"race_name"`
	StationTactical string   `json:"station_tactical"`
	CheckpointCode  string   `json:"checkpoint_code"`
	HQLocalCodes    []string `json:"hq_local_codes"`
	HQCall          string   `json:"hq_call"`
	GWChannel       int      `json:"gw_channel"`
	Path            string   `json:"path"`
	MaxTextLen      int      `json:"max_text_len"`
	FlushAfterSec   int      `json:"flush_after_sec"`
	MaxInFlight     int      `json:"max_in_flight"`
	HeartbeatSec    int      `json:"heartbeat_sec"`
	GapGraceSec     int      `json:"gap_grace_sec"`
}

type settingsView struct {
	settingsBody
	RaceState     string     `json:"race_state"`
	RaceStartedAt *time.Time `json:"race_started_at,omitempty"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

func toSettingsView(c store.Settings) settingsView {
	codes := c.LocalCodes()
	if c.Role != store.RoleHQ {
		codes = nil
	}
	return settingsView{
		settingsBody: settingsBody{
			Role: c.Role, RaceName: c.RaceName, StationTactical: c.StationTactical,
			CheckpointCode: c.CheckpointCode, HQLocalCodes: codes, HQCall: c.HQCall,
			GWChannel: c.GWChannel, Path: c.Path, MaxTextLen: c.MaxTextLen, FlushAfterSec: c.FlushAfterSec,
			MaxInFlight: c.MaxInFlight, HeartbeatSec: c.HeartbeatSec, GapGraceSec: c.GapGraceSec,
		},
		RaceState: c.RaceState, RaceStartedAt: c.RaceStartedAt, UpdatedAt: c.UpdatedAt,
	}
}

func (s *server) getSettings(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsView(cfg))
}

// putSettings saves an edit. The role can change only during setup:
// switching a node between checkpoint and HQ mid-race would strand data.
func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	var b settingsBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cur, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if b.Role != cur.Role && cur.RaceState != store.RaceSetup {
		writeError(w, r, s.log, &httpError{http.StatusConflict, "wrong_state", "the role can only change before the race starts (or after a reset)"})
		return
	}
	up := strings.ToUpper
	codes := make([]string, len(b.HQLocalCodes))
	for i, c := range b.HQLocalCodes {
		codes[i] = up(strings.TrimSpace(c))
	}
	next := store.Settings{
		Role: b.Role, RaceName: strings.TrimSpace(b.RaceName), StationTactical: up(strings.TrimSpace(b.StationTactical)),
		CheckpointCode: up(strings.TrimSpace(b.CheckpointCode)), HQLocalCodes: strings.Join(codes, ","),
		HQCall: up(strings.TrimSpace(b.HQCall)), GWChannel: b.GWChannel, Path: up(strings.ReplaceAll(b.Path, " ", "")),
		MaxTextLen: b.MaxTextLen, FlushAfterSec: b.FlushAfterSec, MaxInFlight: b.MaxInFlight,
		HeartbeatSec: b.HeartbeatSec, GapGraceSec: b.GapGraceSec,
	}
	saved, err := s.Store.UpdateSettings(r.Context(), next)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsView(saved))
}

type callsignBody struct {
	Callsign string `json:"callsign"`
	// Confirm acknowledges that this changes graywolf's station callsign
	// for everything, not just the race (spec 8.1).
	Confirm bool `json:"confirm"`
}

func (s *server) putCallsign(w http.ResponseWriter, r *http.Request) {
	var b callsignBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if !b.Confirm {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "confirm_required",
			"this changes graywolf's station callsign for all of graywolf; confirm to continue"})
		return
	}
	cfg, err := s.Graywolf.SetStationCallsign(r.Context(), b.Callsign)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("web: graywolf station callsign changed", "callsign", cfg.Callsign)
	writeJSON(w, http.StatusOK, map[string]string{"callsign": cfg.Callsign})
}

type graywolfView struct {
	Version          string   `json:"version,omitempty"`
	Callsign         string   `json:"callsign,omitempty"`
	Reachable        bool     `json:"reachable"`
	Connected        bool     `json:"connected"`
	AuthFailed       bool     `json:"auth_failed"`
	MaxText          int      `json:"max_text,omitempty"`
	RetentionDays    int      `json:"retention_days"`
	RetryMaxAttempts int      `json:"retry_max_attempts,omitempty"`
	Warnings         []string `json:"warnings,omitempty"`
}

// getGraywolf is the admin page's graywolf connection panel.
func (s *server) getGraywolf(w http.ResponseWriter, r *http.Request) {
	v := graywolfView{}
	is := s.Inbox.Status()
	v.Connected, v.AuthFailed = is.Connected, is.AuthFailed
	ver, err := s.Graywolf.Version(r.Context())
	if err != nil {
		v.Warnings = append(v.Warnings, "graywolf not reachable: "+safeErr(err))
		writeJSON(w, http.StatusOK, v)
		return
	}
	v.Reachable, v.Version = true, ver.Version
	if ver.Version != testedGraywolfVersion {
		v.Warnings = append(v.Warnings, fmt.Sprintf("graywolf %s is untested (tested: %s)", ver.Version, testedGraywolfVersion))
	}
	if st, err := s.Graywolf.StationConfig(r.Context()); err == nil {
		v.Callsign = st.Callsign
	} else {
		v.Warnings = append(v.Warnings, "station config: "+safeErr(err))
	}
	if p, err := s.Graywolf.MessagePreferences(r.Context()); err == nil {
		v.MaxText, v.RetentionDays, v.RetryMaxAttempts = p.MaxText(), p.RetentionDays, p.RetryMaxAttempts
		if p.RetentionDays > 0 && p.RetentionDays < 2 {
			v.Warnings = append(v.Warnings, fmt.Sprintf("graywolf deletes messages after %d day(s); keep it at 0 (forever) or longer than the race", p.RetentionDays))
		}
	} else {
		v.Warnings = append(v.Warnings, "message preferences: "+safeErr(err))
	}
	writeJSON(w, http.StatusOK, v)
}

// safeErr is a client-safe description of a graywolf client error.
func safeErr(err error) string {
	_, _, msg := classify(err)
	if msg == "internal error" {
		return "unreachable"
	}
	return msg
}

func (s *server) getPeers(w http.ResponseWriter, r *http.Request) {
	ps, err := s.Store.ListPeerPrefs(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	type peer struct {
		Callsign         string    `json:"callsign"`
		OriginalSendPath string    `json:"original_send_path"`
		OriginalWaitAck  bool      `json:"original_wait_for_ack"`
		SavedAt          time.Time `json:"saved_at"`
	}
	out := make([]peer, len(ps))
	for i, p := range ps {
		out[i] = peer{p.Callsign, p.SendPath, p.WaitForAck, p.SavedAt}
	}
	writeJSON(w, http.StatusOK, map[string]any{"peers": out})
}

func (s *server) postPeersRestore(w http.ResponseWriter, r *http.Request) {
	if err := s.Ops.RestorePeers(r.Context()); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type rereadBody struct {
	Since time.Time `json:"since"`
}

// postInboxReread re-reads graywolf's messages from a time (spec 4.6):
// recovery after a wiped database. Rows already processed are skipped.
func (s *server) postInboxReread(w http.ResponseWriter, r *http.Request) {
	var b rereadBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if b.Since.IsZero() || b.Since.After(s.now()) {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "since must be a past RFC 3339 time"})
		return
	}
	if err := s.Store.SetInboxSince(r.Context(), b.Since); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.Inbox.Kick()
	w.WriteHeader(http.StatusAccepted)
}

// --- Race lifecycle (spec 4.7) ---

func (s *server) lifecycle(w http.ResponseWriter, r *http.Request, act func() (store.Settings, error)) {
	cfg, err := act()
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toSettingsView(cfg))
}

func (s *server) postStart(w http.ResponseWriter, r *http.Request) {
	s.lifecycle(w, r, func() (store.Settings, error) { return s.Ops.Start(r.Context()) })
}

func (s *server) postComplete(w http.ResponseWriter, r *http.Request) {
	s.lifecycle(w, r, func() (store.Settings, error) { return s.Ops.Complete(r.Context()) })
}

func (s *server) postCheckIn(w http.ResponseWriter, r *http.Request) {
	s.lifecycle(w, r, func() (store.Settings, error) { return s.Ops.CheckIn(r.Context()) })
}

func (s *server) postSecure(w http.ResponseWriter, r *http.Request) {
	res, err := s.Ops.Secure(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": toSettingsView(res.Settings), "unconfirmed": res.Unconfirmed})
}

func (s *server) postCleanup(w http.ResponseWriter, r *http.Request) {
	res, err := s.Ops.CleanupGraywolf(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"deleted": res.Deleted, "gone": res.Gone, "kept": res.Kept, "failed": res.Failed})
}

type resetBody struct {
	Confirm           string `json:"confirm"`
	ClearReference    bool   `json:"clear_reference"`
	ClearBranding     bool   `json:"clear_branding"`
	AcknowledgeUnsent bool   `json:"acknowledge_unsent"`
}

func (s *server) postReset(w http.ResponseWriter, r *http.Request) {
	var b resetBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	res, err := s.Ops.Reset(r.Context(), ops.ResetRequest{
		Confirm: b.Confirm, ClearReference: b.ClearReference, ClearBranding: b.ClearBranding,
		AcknowledgeUnsent: b.AcknowledgeUnsent,
	})
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backup_dir": res.BackupDir, "warnings": res.Warnings})
}

// --- Checkpoint tools ---

type batchView struct {
	Seq       uint32     `json:"seq"`
	State     string     `json:"state"`
	Attempts  int        `json:"attempts"`
	MsgID     string     `json:"msg_id,omitempty"`
	LastTxAt  *time.Time `json:"last_tx_at,omitempty"`
	NextTxAt  time.Time  `json:"next_tx_at"`
	Text      string     `json:"text"`
	CreatedAt time.Time  `json:"created_at"`
}

// getOutbox is the checkpoint's delivery view.
func (s *server) getOutbox(w http.ResponseWriter, r *http.Request) {
	st, err := s.Store.OutboxStats(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	pending, err := s.Store.ListPendingBatches(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	batches := make([]batchView, len(pending))
	for i, b := range pending {
		batches[i] = batchView{Seq: b.Seq, State: b.State, Attempts: b.Attempts, MsgID: b.GWMsgID,
			LastTxAt: b.LastTxAt, NextTxAt: b.NextTxAt, Text: b.Text, CreatedAt: b.CreatedAt}
	}
	out := map[string]any{"stats": st, "pending": batches}
	if c := s.Checkpoint.LastContact(); !c.IsZero() {
		out["last_hq_contact"] = c
	}
	if rf := s.Checkpoint.LastRefusal(); !rf.At.IsZero() {
		out["refusal"] = map[string]any{"at": rf.At, "reason": rf.Reason}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) getRecoveryExport(w http.ResponseWriter, r *http.Request) {
	var buf bytes.Buffer
	if _, err := s.Ops.ExportCheckpoint(r.Context(), &buf); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	csvDownload(w, "checkpoint-export.csv", buf.Bytes())
}

// csvDownload sends a finished CSV (built in memory first, so an error
// is a clean JSON error, never half a file).
func csvDownload(w http.ResponseWriter, name string, body []byte) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	_, _ = w.Write(body)
}
