package web

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"checkin-board/internal/auth"
	"checkin-board/internal/branding"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/hq"
	"checkin-board/internal/linkcheck"
	"checkin-board/internal/ops"
	"checkin-board/internal/store"
)

// httpError is an error with a chosen status and a client-safe message.
type httpError struct {
	status  int
	code    string
	message string
}

func (e *httpError) Error() string { return e.message }

// errorBody is every error response.
type errorBody struct {
	Error string `json:"error"`
	Code  string `json:"code"`
	// Details carries structured context (e.g. contrast ratios).
	Details any `json:"details,omitempty"`
}

// classify maps an error to a status, code and client-safe message.
// Unknown errors become a generic 500 (the detail is logged, not sent).
func classify(err error) (int, string, string) {
	var he *httpError
	var rl *auth.RateLimitError
	var soon *linkcheck.TooSoonError
	switch {
	case errors.As(err, &soon):
		return http.StatusTooManyRequests, "too_soon", soon.Error()
	case errors.As(err, &he):
		return he.status, he.code, he.message
	case errors.As(err, &rl):
		return http.StatusTooManyRequests, "rate_limited", rl.Error()
	case errors.Is(err, auth.ErrBadCredentials):
		return http.StatusUnauthorized, "bad_credentials", "wrong password"
	case errors.Is(err, auth.ErrNoSession):
		return http.StatusUnauthorized, "login_required", "log in first"
	case errors.Is(err, auth.ErrNotConfigured):
		return http.StatusConflict, "not_configured", "not set up yet: ask the station operator"
	case errors.Is(err, auth.ErrBadSetupCode):
		return http.StatusForbidden, "bad_setup_code", err.Error()
	case errors.Is(err, auth.ErrSetupDone):
		return http.StatusConflict, "setup_done", "setup is already done; log in"
	case errors.Is(err, auth.ErrWeakPassword), errors.Is(err, auth.ErrSamePassword):
		return http.StatusBadRequest, "bad_password", err.Error()
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not_found", "not found"
	case errors.Is(err, store.ErrAlreadyVoided):
		return http.StatusConflict, "already_voided", "already voided"
	case errors.Is(err, store.ErrConflict):
		return http.StatusConflict, "conflict", "already exists"
	case errors.Is(err, store.ErrInvalidInput), errors.Is(err, store.ErrInvalidSettings),
		errors.Is(err, branding.ErrInvalid), errors.Is(err, branding.ErrBadLogo):
		return http.StatusBadRequest, "invalid", err.Error()
	case errors.Is(err, ops.ErrConfirmMismatch):
		return http.StatusBadRequest, "confirm_mismatch", err.Error()
	case errors.Is(err, ops.ErrWrongState):
		return http.StatusConflict, "wrong_state", err.Error()
	case errors.Is(err, ops.ErrWrongRole):
		return http.StatusConflict, "wrong_role", "not available in this node's role"
	case errors.Is(err, ops.ErrUnsentData):
		return http.StatusConflict, "unsent_data", err.Error()
	case errors.Is(err, linkcheck.ErrNeedsConfirm):
		return http.StatusConflict, "confirm_needed", "the race is active: confirm to spend airtime on a link check"
	case errors.Is(err, linkcheck.ErrBusy):
		return http.StatusConflict, "busy", "a link check is already running"
	case errors.Is(err, linkcheck.ErrNotAllowed):
		return http.StatusConflict, "not_allowed", err.Error()
	case errors.Is(err, linkcheck.ErrInvalid):
		return http.StatusBadRequest, "invalid", err.Error()
	case errors.Is(err, hq.ErrTooSoon):
		return http.StatusTooManyRequests, "too_soon", "just asked; try again in a minute"
	case errors.Is(err, graywolf.ErrAuth):
		return http.StatusBadGateway, "graywolf_auth", "graywolf rejected the app's login"
	}
	var apiErr *graywolf.APIError
	if errors.As(err, &apiErr) {
		return http.StatusBadGateway, "graywolf_error", "graywolf: " + apiErr.Message
	}
	return http.StatusInternalServerError, "internal", "internal error"
}

// writeError logs server-side failures in full and sends a safe body.
func writeError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	status, code, msg := classify(err)
	if status >= 500 {
		log.Error("web: request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", err)
	}
	body := errorBody{Error: msg, Code: code}
	var ve *branding.ValidationError
	if errors.As(err, &ve) {
		body.Details = ve.Contrasts
	}
	var ie *ops.ImportError
	if errors.As(err, &ie) {
		body.Details = ie.Rows
	}
	var rl *auth.RateLimitError
	if errors.As(err, &rl) {
		w.Header().Set("Retry-After", strconv.Itoa(int(rl.RetryAfter.Seconds()+0.999)))
	}
	var soon *linkcheck.TooSoonError
	if errors.As(err, &soon) {
		w.Header().Set("Retry-After", strconv.Itoa(int(soon.RetryAfter.Seconds()+0.999)))
	}
	writeJSON(w, status, body)
}
