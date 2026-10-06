package web

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"checkin-board/internal/linkcheck"
)

// The automation hook lets graywolf's own Actions run a link check: net
// control sends @@<otp>#linkcheck to a checkpoint's graywolf, whose
// webhook Action POSTs here and relays the one-line result on air.
// (graywolf's command Actions can't switch to this app's user.) It is
// off unless CB_HOOK_TOKEN_FILE is set, answers only loopback callers
// with the bearer token, and is bounded by the link check's own limits.

// hookMaxWait leaves room for a 10-probe run plus the reply wait; the
// Action's timeout should be longer (README: 300 s).
const hookMaxWait = 4 * time.Minute

func (s *server) checkHook(r *http.Request) error {
	if s.HookToken == "" {
		return &httpError{http.StatusNotFound, "not_found", "not found"}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); err != nil || ip == nil || !ip.IsLoopback() {
		return &httpError{http.StatusForbidden, "forbidden", "the automation hook only answers this node"}
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(s.HookToken)) != 1 {
		return &httpError{http.StatusUnauthorized, "bad_token", "bad token"}
	}
	return nil
}

// postHookLinkCheck runs a link check and answers one short text line,
// always 200: graywolf relays a 2xx body on air, but only "error: http
// NNN" for anything else, which would hide the reason.
func (s *server) postHookLinkCheck(w http.ResponseWriter, r *http.Request) {
	var b linkCheckBody
	if r.ContentLength != 0 {
		if err := decodeJSON(r, &b); err != nil {
			writeHookLine(w, "bad request")
			return
		}
	}
	tm := s.LinkTiming
	if tm == (linkcheck.Timing{}) {
		tm = linkcheck.DefaultTiming
		tm.MaxWait = hookMaxWait
	}
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(tm.MaxWait + 30*time.Second)); err != nil {
		s.log.Warn("web: hook write deadline", "err", err)
	}
	// Never during the race from a radio command: no confirm here.
	c, err := linkcheck.Request(r.Context(), s.Store, linkcheck.Req{To: b.To, Count: b.Count, Source: "action"}, s.now())
	if err == nil {
		c, err = linkcheck.Await(r.Context(), s.Store, c.ID, tm)
	}
	if err != nil {
		line, known := hookError(err)
		if !known {
			s.log.Error("web: hook link check", "err", err)
		}
		writeHookLine(w, line)
		return
	}
	writeHookLine(w, linkcheck.Brief(c))
}

// hookError is the on-air text for a refusal. Only the link check's own
// messages go on air; anything else (a database error) is a fixed text
// and known reports false so the caller logs the detail.
func hookError(err error) (line string, known bool) {
	var soon *linkcheck.TooSoonError
	switch {
	case errors.As(err, &soon):
		return fmt.Sprintf("runs are 2 min apart; retry in %ds", int(soon.RetryAfter.Seconds()+0.999)), true
	case errors.Is(err, linkcheck.ErrNeedsConfirm):
		return "race active: run it from the admin page", true
	case errors.Is(err, linkcheck.ErrBusy):
		return "a link check is already running", true
	case errors.Is(err, linkcheck.ErrNotPickedUp):
		return "service not running the check", true
	case errors.Is(err, linkcheck.ErrNotAllowed), errors.Is(err, linkcheck.ErrInvalid):
		return strings.TrimPrefix(err.Error(), "linkcheck: "), true
	}
	return "couldn't run the check (see the node's log)", false
}

func writeHookLine(w http.ResponseWriter, line string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintln(w, line)
}
