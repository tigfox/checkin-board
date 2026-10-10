package web

import (
	"bytes"
	"checkin-board/internal/hostmon"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/linkcheck"
	"checkin-board/internal/panel"
	"checkin-board/internal/panel/epd"
	"checkin-board/internal/panel/menu"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

// Node panel (spec 8.4). The admin edits its settings and menu here; the
// panel service reads its view and runs menu items over the local hook.

// panelFloor is the e-ink full-refresh floor the app remembers, so a
// restarting panel can't refresh faster than every 3 minutes.
const panelFloor = 3 * time.Minute

// panelRequests are admin requests the panel picks up on its next poll.
// They live in memory, so they start over when the app restarts: Boot
// tells the panel so, and it doesn't replay them.
type panelRequests struct {
	mu          sync.Mutex
	boot        int64
	refreshSeq  int64
	testPattern string
	testSeq     int64
}

func (q *panelRequests) snapshot() (boot, refresh int64, pattern string, test int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.boot, q.refreshSeq, q.testPattern, q.testSeq
}

type panelSettingsView struct {
	Enabled     bool       `json:"enabled"`
	Controller  string     `json:"controller"`
	RefreshMin  int        `json:"refresh_min"`
	Rotation    int        `json:"rotation"`
	LastFullAt  *time.Time `json:"last_full_at,omitempty"`
	Controllers []string   `json:"controllers"`
	MinRefresh  int        `json:"min_refresh"`
	MaxRefresh  int        `json:"max_refresh"`
}

func toPanelSettingsView(p store.PanelSettings) panelSettingsView {
	return panelSettingsView{Enabled: p.Enabled, Controller: p.Controller, RefreshMin: p.RefreshMin, Rotation: p.Rotation,
		LastFullAt: p.LastFullAt, Controllers: epd.Controllers(), MinRefresh: store.PanelMinRefreshMin, MaxRefresh: store.PanelMaxRefreshMin}
}

func (s *server) getPanel(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetPanelSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, toPanelSettingsView(p))
}

type panelSettingsBody struct {
	Enabled    bool   `json:"enabled"`
	Controller string `json:"controller"`
	RefreshMin int    `json:"refresh_min"`
	Rotation   int    `json:"rotation"`
}

func (s *server) putPanel(w http.ResponseWriter, r *http.Request) {
	var b panelSettingsBody
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	p, err := s.Store.SavePanelSettings(r.Context(), store.PanelSettings{Enabled: b.Enabled, Controller: b.Controller,
		RefreshMin: b.RefreshMin, Rotation: b.Rotation})
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("panel: settings changed by admin", "enabled", p.Enabled, "controller", p.Controller, "refresh_min", p.RefreshMin)
	writeJSON(w, http.StatusOK, toPanelSettingsView(p))
}

type panelActionInfo struct {
	Action    menu.Action `json:"action"`
	Roles     string      `json:"roles"`
	Lifecycle bool        `json:"lifecycle"`
}

type panelMenuView struct {
	Items   []menu.Item       `json:"items"`
	Edited  bool              `json:"edited"`
	Actions []panelActionInfo `json:"actions"`
	MaxItem int               `json:"max_items"`
	MaxLen  int               `json:"max_label"`
}

func (s *server) writePanelMenu(w http.ResponseWriter, r *http.Request) {
	items, edited, err := s.Store.PanelMenu(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	v := panelMenuView{Items: items, Edited: edited, MaxItem: menu.MaxItems, MaxLen: menu.MaxLabel}
	for _, a := range menu.Actions() {
		v.Actions = append(v.Actions, panelActionInfo{Action: a, Roles: a.Roles(), Lifecycle: a.Lifecycle()})
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) getPanelMenu(w http.ResponseWriter, r *http.Request) { s.writePanelMenu(w, r) }

func (s *server) putPanelMenu(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Items []menu.Item `json:"items"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	items, err := menu.Validate(b.Items)
	if err != nil {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", strings.TrimPrefix(err.Error(), "menu: invalid: ")})
		return
	}
	if err := s.Store.ReplacePanelMenu(r.Context(), items); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("panel: menu edited by admin", "items", len(items))
	s.writePanelMenu(w, r)
}

func (s *server) postPanelMenuReset(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.ResetPanelMenu(r.Context()); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.writePanelMenu(w, r)
}

func (s *server) postPanelRefresh(w http.ResponseWriter, r *http.Request) {
	s.panelReq.mu.Lock()
	s.panelReq.refreshSeq++
	s.panelReq.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"message": "The panel refreshes at its next poll (within 30 s), or as soon as its 3-minute rest allows."})
}

func (s *server) postPanelTestPattern(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Controller string `json:"controller"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if !epd.Known(b.Controller) {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "unknown controller"})
		return
	}
	s.panelReq.mu.Lock()
	s.panelReq.testPattern = b.Controller
	s.panelReq.testSeq++
	s.panelReq.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"message": "The panel shows the test pattern at its next poll (within 30 s)."})
}

func (s *server) postPanelDetect(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.SetPanelController(r.Context(), ""); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "At the node, the panel tries each controller in turn: press a button when the text is readable."})
}

// getPanelPreview renders the status screen as the panel would show it.
// The node's addresses are only known to the panel process.
func (s *server) getPanelPreview(w http.ResponseWriter, r *http.Request) {
	v, err := s.panelView(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	img := panel.Rotate(panel.StatusScreen(v, []string{"<node address>"}, hostmon.Snapshot{}), v.Settings.Rotation)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// panelView builds what the panel shows.
func (s *server) panelView(r *http.Request) (panel.View, error) {
	ctx := r.Context()
	cfg, err := s.Store.GetSettings(ctx)
	if err != nil {
		return panel.View{}, err
	}
	ps, err := s.Store.GetPanelSettings(ctx)
	if err != nil {
		return panel.View{}, err
	}
	items, _, err := s.Store.PanelMenu(ctx)
	if err != nil {
		return panel.View{}, err
	}
	st := panel.Status{Role: cfg.Role, RaceName: cfg.RaceName, RaceState: cfg.RaceState,
		StateLabel: stateLabel(cfg.RaceState, cfg.Role), Station: cfg.StationTactical, CPCode: cfg.CheckpointCode,
		Port: s.WebPort, Now: s.now()}
	gw := bannerFor(s.Inbox.Status())
	st.GraywolfOK, st.GraywolfProblem = gw.Problem == "", gw.Problem
	if s.Clock.Status().Source == raceclock.SourceUnsynced {
		st.Warnings = append(st.Warnings, "Race clock not set")
	}
	if js := s.Ops.JournalStatus(); js.LastError != "" {
		st.Warnings = append(st.Warnings, "Backup journal failing")
	}
	if checks, err := s.Store.ListLinkChecks(ctx, 5); err == nil {
		for _, c := range checks {
			if c.State == store.LinkCheckDone && c.FinishedAt != nil {
				st.LastLink = &panel.Link{Verdict: c.Verdict, At: *c.FinishedAt, Peer: c.PeerCall,
					Uplink: max(c.Uplink, c.RoundTrip), RoundTrip: c.RoundTrip, Count: c.Count}
				break
			}
		}
	}
	v := panel.View{MenuRev: ps.MenuRev, Settings: panel.Settings{Enabled: ps.Enabled, Controller: ps.Controller,
		DetectPending: ps.Controller == "" && !ps.DetectGaveUp, RefreshMin: ps.RefreshMin, Rotation: ps.Rotation}}
	switch cfg.Role {
	case store.RoleCheckpoint:
		ob, err := s.Store.OutboxStats(ctx)
		if err != nil {
			return panel.View{}, err
		}
		st.Unconfirmed = ob.Unconfirmed
		if c := s.Checkpoint.LastContact(); !c.IsZero() {
			st.LastHQContact = &c
		}
	case store.RoleHQ:
		health, err := s.HQ.Health(ctx)
		if err != nil {
			return panel.View{}, err
		}
		hq := &panel.HQ{}
		for _, h := range health {
			if h.Defined {
				hq.Listed++
			}
			if h.LastHeardAt != nil {
				hq.Heard++
			}
			if h.ClosedAt != nil {
				hq.Closed++
			}
			hq.Gaps += len(h.Missing)
		}
		st.HQ = hq
		cps, err := s.Store.ListCheckpoints(ctx)
		if err != nil {
			return panel.View{}, err
		}
		for _, c := range cps {
			v.Targets = append(v.Targets, panel.Target{Code: c.Code, Name: c.Name, Call: c.ExpectedCall})
		}
	}
	v.Status = st
	for _, it := range items {
		v.Menu = append(v.Menu, panel.MenuItem{Item: it, Available: it.Available(cfg.Role, cfg.RaceState)})
	}
	if ps.LastFullAt != nil {
		at := ps.LastFullAt.Add(panelFloor)
		v.FullAllowedAt = &at
	}
	v.Boot, v.RefreshSeq, v.TestPattern, v.TestPatternSeq = s.panelReq.snapshot()
	return v, nil
}

// stateLabel matches the web UI's wording (logic.js stateLabel).
func stateLabel(state, role string) string {
	if role == store.RoleCheckpoint {
		switch state {
		case store.RaceSetup:
			return "Checkpoint not open"
		case store.RaceActive:
			return "Checkpoint open"
		case store.RaceComplete:
			return "Checkpoint closed"
		}
	}
	switch state {
	case store.RaceSetup:
		return "Race not started"
	case store.RaceActive:
		return "Race active"
	case store.RaceComplete:
		return "Race complete"
	case store.RaceSecured:
		return "Secured for travel"
	case store.RaceCheckingIn:
		return "Final check-in"
	case store.RaceCheckedIn:
		return "Checked in"
	}
	return state
}

func (s *server) getHookPanel(w http.ResponseWriter, r *http.Request) {
	v, err := s.panelView(r)
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *server) postHookPanelRefreshed(w http.ResponseWriter, r *http.Request) {
	var b struct {
		At time.Time `json:"at"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	at := b.At
	if at.IsZero() || at.After(s.now().Add(time.Minute)) {
		at = s.now() // a skewed panel clock never pushes the floor out
	}
	if err := s.Store.MarkPanelFullRefresh(r.Context(), at); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) putHookPanelController(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Controller string `json:"controller"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if !epd.Known(b.Controller) {
		writeError(w, r, s.log, &httpError{http.StatusBadRequest, "invalid", "unknown controller"})
		return
	}
	if err := s.Store.SetPanelController(r.Context(), b.Controller); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Info("panel: controller detected", "controller", b.Controller)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) postHookPanelGaveUp(w http.ResponseWriter, r *http.Request) {
	var b struct {
		GaveUp bool `json:"gave_up"`
	}
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	if err := s.Store.PanelDetectionGaveUp(r.Context()); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	s.log.Warn("panel: no display controller confirmed at the node; choose one or detect again on Admin > Panel")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// postHookPanelAction runs a menu item. Refusals are 200 with ok false
// and a reason for the panel to show; the app checks the item exists, is
// enabled, matches the action the panel saw, and is available now.
func (s *server) postHookPanelAction(w http.ResponseWriter, r *http.Request) {
	refuse := func(msg string) { writeJSON(w, http.StatusOK, panel.ActionResult{Message: msg}) }
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		refuse("Unknown menu item.")
		return
	}
	var b panel.ActionRequest
	if err := decodeJSON(r, &b); err != nil {
		writeError(w, r, s.log, err)
		return
	}
	cfg, err := s.Store.GetSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	items, _, err := s.Store.PanelMenu(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	ps, err := s.Store.GetPanelSettings(r.Context())
	if err != nil {
		writeError(w, r, s.log, err)
		return
	}
	var it *menu.Item
	for i := range items {
		if items[i].ID == id {
			it = &items[i]
		}
	}
	switch {
	case it == nil || it.Action != b.Action || b.MenuRev != ps.MenuRev:
		refuse("The menu changed; try again.")
		return
	case it.Action.Local():
		refuse("That item works on the panel itself.")
		return
	case !it.Available(cfg.Role, cfg.RaceState):
		refuse("Not available now (" + stateLabel(cfg.RaceState, cfg.Role) + ").")
		return
	}
	s.log.Info("panel: action", "source", "panel", "item", it.Label, "action", it.Action, "to", b.To)
	msg, err := s.runPanelAction(r, cfg, it.Action, b.To)
	if err != nil {
		status, _, text := classify(err)
		if status >= 500 {
			s.log.Error("panel: action failed", "action", it.Action, "err", err)
		}
		refuse(text)
		return
	}
	writeJSON(w, http.StatusOK, panel.ActionResult{OK: true, Message: msg})
}

func (s *server) runPanelAction(r *http.Request, cfg store.Settings, a menu.Action, to string) (string, error) {
	ctx := r.Context()
	cp := cfg.Role == store.RoleCheckpoint
	switch a {
	case menu.LinkCheck:
		c, err := linkcheck.Request(ctx, s.Store, linkcheck.Req{To: to, Confirm: true, Source: "panel"}, s.now())
		if err != nil {
			var soon *linkcheck.TooSoonError
			if errors.As(err, &soon) {
				return "", &httpError{http.StatusTooManyRequests, "too_soon", fmt.Sprintf("Runs are 2 min apart: try in %d s.", int(soon.RetryAfter.Seconds()+0.999))}
			}
			return "", err
		}
		return fmt.Sprintf("Started: probing %s. Result in 1-3 min.", c.PeerCall), nil
	case menu.StartRace:
		if _, err := s.Ops.Start(ctx); err != nil {
			return "", err
		}
		if cp {
			return "Checkpoint open: the keypad takes entries.", nil
		}
		return "Race started.", nil
	case menu.CompleteRace:
		if _, err := s.Ops.Complete(ctx); err != nil {
			return "", err
		}
		if cp {
			return "Checkpoint closed. Entries keep going out.", nil
		}
		return "Race complete.", nil
	case menu.Secure:
		res, err := s.Ops.Secure(ctx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Secured: %d entries go out at HQ.", res.Unconfirmed), nil
	case menu.CheckIn:
		if _, err := s.Ops.CheckIn(ctx); err != nil {
			return "", err
		}
		return "Checking in: sending everything to HQ.", nil
	}
	return "", &httpError{http.StatusBadRequest, "invalid", "not a panel action"}
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }
