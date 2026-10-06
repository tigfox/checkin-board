package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"checkin-board/internal/inbox"
	"checkin-board/internal/ops"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

const (
	defaultEntryLimit = 20
	maxEntryLimit     = 500
)

type stationView struct {
	Role            string        `json:"role"`
	RaceName        string        `json:"race_name"`
	RaceState       string        `json:"race_state"`
	StationTactical string        `json:"station_tactical"`
	LocalCodes      []string      `json:"local_codes"`
	Queued          int           `json:"queued"`
	Unconfirmed     int           `json:"unconfirmed"`
	LastHQContact   *time.Time    `json:"last_hq_contact,omitempty"`
	Journal         journalView   `json:"journal"`
	Graywolf        graywolfState `json:"graywolf"`
}

type journalView struct {
	Enabled   bool   `json:"enabled"`
	LastError string `json:"last_error,omitempty"`
}

// graywolfState is the volunteer banner: a plain problem summary, never
// raw error text (which carries graywolf's internal address).
type graywolfState struct {
	Connected   bool   `json:"connected"`
	AuthFailed  bool   `json:"auth_failed"`
	Problem     string `json:"problem,omitempty"`
	SkippedRows int    `json:"skipped_rows"`
}

func bannerFor(is inbox.Status) graywolfState {
	g := graywolfState{Connected: is.Connected, AuthFailed: is.AuthFailed, SkippedRows: is.SkippedRows}
	switch {
	case is.AuthFailed:
		g.Problem = "graywolf rejected the app's login"
	case is.StreamError != "" || is.CatchUpError != "":
		g.Problem = "graywolf unreachable"
	}
	return g
}

// getStation is the volunteer header: race, state, delivery and
// graywolf health (the "graywolf unreachable" banner).
func (s *server) getStation(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	v := stationView{
		Role: cfg.Role, RaceName: cfg.RaceName, RaceState: cfg.RaceState,
		StationTactical: cfg.StationTactical, LocalCodes: cfg.LocalCodes(),
	}
	if cfg.Role == store.RoleCheckpoint {
		st, err := s.Store.OutboxStats(r.Context())
		if err != nil {
			writeError(w, r, s.log, err)
			return
		}
		v.Queued, v.Unconfirmed = st.Queued, st.Unconfirmed
		if c := s.Checkpoint.LastContact(); !c.IsZero() {
			v.LastHQContact = &c
		}
	}
	js := s.Ops.JournalStatus()
	v.Journal = journalView{Enabled: js.Enabled, LastError: js.LastError}
	v.Graywolf = bannerFor(s.Inbox.Status())
	writeJSON(w, http.StatusOK, v)
}

func (s *server) getEntries(w http.ResponseWriter, r *http.Request) {
	limit := defaultEntryLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxEntryLimit {
			writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "limit must be 1-500"})
			return
		}
		limit = n
	}
	entries, err := s.Ops.RecentEntries(r.Context(), limit)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

type entryBody struct {
	CP  string `json:"cp"`  // optional on a checkpoint (its own code)
	Bib string `json:"bib"` // as typed: "0042" is 42
	// RequestID (optional) makes a retry safe: a keypad that lost the
	// response resends the same id and gets the first result back
	// instead of logging the bib twice (spec 8.2).
	RequestID string `json:"request_id"`
}

type entryResult struct {
	ID          uint      `json:"id,omitempty"`
	TimeIn      time.Time `json:"time_in"`
	ClockSynced bool      `json:"clock_synced"`
	// JournalOnly: the database failed but the entry is safe in the bib
	// journal. The keypad must say "saved; don't re-enter".
	JournalOnly bool   `json:"journal_only,omitempty"`
	Warning     string `json:"warning,omitempty"`
}

func (s *server) postEntry(w http.ResponseWriter, r *http.Request) {
	var b entryBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	bib, err := wire.ParseBib(b.Bib)
	if err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid_bib", "bib must be 1-9999"})
		return
	}
	if len(b.RequestID) > maxRequestIDLen {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "request_id too long"})
		return
	}
	if b.RequestID != "" {
		release, prior, ok := s.dedup.begin(b.RequestID, fmt.Sprintf("%d|%s", bib, b.CP))
		if !ok {
			writeJSON(w, prior.status, prior.result) // a retry of a logged entry
			return
		}
		rec := &recordingWriter{ResponseWriter: w}
		defer func() { release(rec.status, rec.body.Bytes()) }()
		w = rec
	}
	cp := b.CP
	if cp == "" {
		cfg, err := s.Store.GetSettings(r.Context())
		if err != nil {
			writeError(w, r, s.log, err)
			return
		}
		if codes := cfg.LocalCodes(); len(codes) > 0 {
			cp = codes[0]
		}
	}
	// A client that disconnects mid-save must not leave the outcome
	// ambiguous (committed, but reported as failed and not cached).
	res, err := s.Ops.LogBib(context.WithoutCancel(r.Context()), cp, bib)
	if errors.Is(err, ops.ErrJournalOnly) {
		writeJSON(w, http.StatusAccepted, entryResult{TimeIn: res.TimeIn, ClockSynced: res.ClockSynced,
			JournalOnly: true, Warning: "saved in the backup journal only; do not re-enter it"})
		return
	}
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	out := entryResult{ID: res.ID, TimeIn: res.TimeIn, ClockSynced: res.ClockSynced}
	if res.JournalErr != nil {
		out.Warning = "saved, but the backup journal could not be written"
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *server) deleteEntry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 32)
	if err != nil || id == 0 {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "bad entry id"})
		return
	}
	res, err := s.Ops.VoidBib(r.Context(), uint(id))
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if res.JournalErr != nil {
		writeJSON(w, http.StatusOK, map[string]string{"warning": "voided, but the backup journal could not be written"})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type clockView struct {
	Source    string    `json:"source"` // unsynced | system | browser
	Now       time.Time `json:"now"`
	SyncAgeMS int64     `json:"sync_age_ms"`
}

func (s *server) clockView() clockView {
	st := s.Clock.Status()
	return clockView{Source: string(st.Source), Now: st.Now.UTC(), SyncAgeMS: st.SyncAge.Milliseconds()}
}

func (s *server) getClock(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.clockView())
}

type clockSyncBody struct {
	ClientUnixMS int64 `json:"client_unix_ms"`
	RTTMS        int64 `json:"rtt_ms"` // the browser's previous round trip
}

// postClockSync sets the race clock from a volunteer's browser (spec 6).
func (s *server) postClockSync(w http.ResponseWriter, r *http.Request) {
	var b clockSyncBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	err := s.Clock.Sync(time.UnixMilli(b.ClientUnixMS), time.Duration(b.RTTMS)*time.Millisecond)
	if err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.clockView())
}
