package web

import (
	"net/http"
	"time"

	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
)

// Link check (spec 4.8). Requests are recorded in the store; the app's
// link-check service runs them.

const linkHistory = 20

type linkCheckView struct {
	ID            uint       `json:"id"`
	PeerCall      string     `json:"peer_call"`
	Count         int        `json:"count"`
	SpacingSec    int        `json:"spacing_sec"`
	State         string     `json:"state"`
	Source        string     `json:"source"`
	RequestedAt   time.Time  `json:"requested_at"`
	StartedAt     *time.Time `json:"started_at,omitempty"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	Verdict       string     `json:"verdict"`
	Uplink        int        `json:"uplink"`
	RoundTrip     int        `json:"round_trip"`
	ReplyReceived bool       `json:"reply_received"`
	MedianRTTms   *int       `json:"median_rtt_ms,omitempty"`
	RemoteLevel   *int       `json:"remote_level,omitempty"`
	LocalLevel    *int       `json:"local_level,omitempty"`
	Via           string     `json:"via"`
	Advice        string     `json:"advice"`
	Error         string     `json:"error"`
	ProbesSent    int        `json:"probes_sent"`
}

type linkResponseView struct {
	PeerCall    string    `json:"peer_call"`
	ProberCode  string    `json:"prober_code"`
	Heard       int       `json:"heard"`
	Total       int       `json:"total"`
	LastHeardAt time.Time `json:"last_heard_at"`
	Level       *int      `json:"level,omitempty"`
	Via         string    `json:"via"`
	UnknownPeer bool      `json:"unknown_peer"`
	Replied     bool      `json:"replied"`
	ReplyAcked  bool      `json:"reply_acked"`
	Verdict     string    `json:"verdict"`
}

type linkCheckList struct {
	Checks         []linkCheckView    `json:"checks"`
	Responses      []linkResponseView `json:"responses"`
	DefaultCount   int                `json:"default_count"`
	MaxCount       int                `json:"max_count"`
	MinIntervalSec int                `json:"min_interval_sec"`
}

func toLinkCheckView(c store.LinkCheck, probesSent int) linkCheckView {
	return linkCheckView{ID: c.ID, PeerCall: c.PeerCall, Count: c.Count, SpacingSec: c.SpacingSec, State: c.State,
		Source: c.Source, RequestedAt: c.RequestedAt, StartedAt: c.StartedAt, FinishedAt: c.FinishedAt,
		Verdict: c.Verdict, Uplink: c.Uplink, RoundTrip: c.RoundTrip, ReplyReceived: c.ReplyReceived,
		MedianRTTms: c.MedianRTTms, RemoteLevel: c.RemoteLevel, LocalLevel: c.LocalLevel, Via: c.Via,
		Advice: c.Advice, Error: c.Error, ProbesSent: probesSent}
}

func toLinkResponseView(r store.LinkResponse) linkResponseView {
	heard := len(r.HeardIndices())
	return linkResponseView{PeerCall: r.PeerCall, ProberCode: r.ProberCode, Heard: heard, Total: r.Total,
		LastHeardAt: r.LastHeardAt, Level: r.Level, Via: r.Via, UnknownPeer: r.UnknownPeer,
		Replied: r.ReplySentAt != nil, ReplyAcked: r.ReplyAckedAt != nil,
		Verdict: linkcheck.ResponderVerdict(r.Total, heard, r.ReplyAckedAt != nil)}
}

func (s *server) getLinkChecks(w http.ResponseWriter, r *http.Request) {
	checks, err := s.Store.ListLinkChecks(r.Context(), linkHistory)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	responses, err := s.Store.ListLinkResponses(r.Context(), linkHistory)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	out := linkCheckList{Checks: make([]linkCheckView, len(checks)), Responses: make([]linkResponseView, len(responses)),
		DefaultCount: linkcheck.DefaultCount, MaxCount: linkcheck.MaxCount, MinIntervalSec: int(linkcheck.MinInterval / time.Second)}
	for i, c := range checks {
		sent := 0
		if c.State == store.LinkCheckRunning {
			probes, err := s.Store.ListLinkProbes(r.Context(), c.ID)
			if err != nil {
				writeError(w, r, s.log, err)
				return
			}
			sent = len(probes)
		}
		out.Checks[i] = toLinkCheckView(c, sent)
	}
	for i, resp := range responses {
		out.Responses[i] = toLinkResponseView(resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type linkCheckBody struct {
	To      string `json:"to"`
	Count   int    `json:"count"`
	Confirm bool   `json:"confirm"`
}

func (s *server) postLinkCheck(w http.ResponseWriter, r *http.Request) {
	var b linkCheckBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	c, err := linkcheck.Request(r.Context(), s.Store, linkcheck.Req{To: b.To, Count: b.Count, Confirm: b.Confirm, Source: "admin"}, s.now())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusCreated, toLinkCheckView(c, 0))
}

func (s *server) postLinkCheckCancel(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if _, err := s.Store.GetLinkCheck(r.Context(), id); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	ok, err := s.Store.CancelLinkCheck(r.Context(), id, "cancelled by the operator")
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"cancelled": ok})
}

// linkInputs loads what the readiness and health summaries need.
func (s *server) linkInputs(r *http.Request) ([]store.Checkpoint, []store.LinkCheck, []store.LinkResponse, error) {
	cps, err := s.Store.ListCheckpoints(r.Context())
	if err != nil {
		return nil, nil, nil, err
	}
	checks, err := s.Store.ListLinkChecks(r.Context(), 200)
	if err != nil {
		return nil, nil, nil, err
	}
	responses, err := s.Store.ListLinkResponses(r.Context(), 200)
	return cps, checks, responses, err
}

func (s *server) getLinkReadiness(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cps, checks, responses, err := s.linkInputs(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	warnings := linkcheck.Readiness(cfg, cps, checks, responses, s.now())
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string][]string{"warnings": warnings})
}
