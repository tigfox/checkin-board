// Package web is checkin-board's HTTP server: the REST API (spec 7.1)
// behind the app's own two logins (spec 7.2), and the embedded UI.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"checkin-board/internal/auth"
	"checkin-board/internal/checkpoint"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/hq"
	"checkin-board/internal/inbox"
	"checkin-board/internal/linkcheck"
	"checkin-board/internal/ops"
	"checkin-board/internal/raceclock"
	"checkin-board/internal/store"
)

//go:embed static
var staticFS embed.FS

// Body size caps.
const (
	maxJSONBody   = 64 << 10
	maxUploadBody = 8 << 20 // CSV imports and logos (logos are capped again at 1 MB)
	sessionCookie = "cb_session"
)

// Graywolf is what the admin pages read from (and set in) graywolf.
type Graywolf interface {
	Version(ctx context.Context) (graywolf.Version, error)
	StationConfig(ctx context.Context) (graywolf.StationConfig, error)
	SetStationCallsign(ctx context.Context, callsign string) (graywolf.StationConfig, error)
	MessagePreferences(ctx context.Context) (graywolf.MessagePreferences, error)
}

// CheckpointEngine is the checkpoint engine's status side.
type CheckpointEngine interface {
	LastContact() time.Time
	LastRefusal() checkpoint.Refusal
}

// InboxReader is the inbox reader's status side.
type InboxReader interface {
	Status() inbox.Status
	Kick()
}

// Deps are the server's collaborators.
type Deps struct {
	Store      *store.Store
	Auth       *auth.Service
	Ops        *ops.Service
	HQ         *hq.Engine
	Checkpoint CheckpointEngine
	Inbox      InboxReader
	Clock      *raceclock.Clock
	Graywolf   Graywolf
	Logger     *slog.Logger
	Now        func() time.Time
	// Static overrides the embedded UI (tests).
	Static fs.FS
	// HookToken enables the local automation hook (a graywolf webhook
	// Action that runs a link check); "" leaves it off.
	HookToken string
	// LinkTiming bounds the hook's wait for a link check (zero: default).
	LinkTiming linkcheck.Timing
}

type server struct {
	Deps
	log   *slog.Logger
	now   func() time.Time
	dedup *requestDedup
}

// NewHandler builds the HTTP handler.
func NewHandler(d Deps) (http.Handler, error) {
	if d.Store == nil || d.Auth == nil || d.Ops == nil || d.HQ == nil || d.Checkpoint == nil ||
		d.Inbox == nil || d.Clock == nil || d.Graywolf == nil {
		return nil, errors.New("web: missing dependency")
	}
	s := &server{Deps: d, log: d.Logger, now: d.Now}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	s.dedup = newRequestDedup(s.now)
	static := d.Static
	if static == nil {
		sub, err := fs.Sub(staticFS, "static")
		if err != nil {
			return nil, err
		}
		static = sub
	}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		mux.Handle(rt.method+" "+rt.pattern, s.guard(rt))
	}
	mux.Handle("GET /", http.FileServerFS(static))
	return s.recoverer(securityHeaders(mux)), nil
}

// route is one API endpoint and who may call it.
type route struct {
	method, pattern string
	access          access
	h               http.HandlerFunc
	upload          bool // accepts multipart/form-data
}

type access int

const (
	public    access = iota // login, setup
	volunteer               // volunteer or admin session
	admin                   // admin session only
	hook                    // local automation: bearer token from loopback, no session
)

type ctxKey struct{}

// guard applies body limits, the cross-site checks and the role check.
func (s *server) guard(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := int64(maxJSONBody)
		if rt.upload {
			limit = maxUploadBody
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if err := sameSite(r, rt.upload); err != nil {
				writeError(w, r, s.log, &httpError{http.StatusForbidden, "forbidden", err.Error()})
				return
			}
		}
		if rt.access == hook {
			if err := s.checkHook(r); err != nil {
				writeError(w, r, s.log, err)
				return
			}
			rt.h(w, r)
			return
		}
		if rt.access != public {
			role, err := s.Auth.Authenticate(r.Context(), sessionToken(r))
			switch {
			case errors.Is(err, auth.ErrNoSession):
				writeError(w, r, s.log, &httpError{http.StatusUnauthorized, "login_required", "log in first"})
				return
			case err != nil:
				writeError(w, r, s.log, err)
				return
			case rt.access == admin && role != store.RoleAdmin:
				writeError(w, r, s.log, &httpError{http.StatusForbidden, "admin_required", "this needs the admin login"})
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, role))
		}
		rt.h(w, r)
	})
}

// sameSite is CSRF defence in depth on top of SameSite=Strict cookies:
// a state-changing request must be JSON (or, for uploads, multipart),
// which a cross-site HTML form can't send as JSON, and any Origin header
// must match the host.
func sameSite(r *http.Request, upload bool) error {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return errors.New("cross-site request refused")
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return errors.New("cross-origin request refused")
		}
	}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
	case upload && strings.HasPrefix(ct, "multipart/form-data"):
	case r.ContentLength == 0 && ct == "":
		// Bodyless actions (e.g. logout, start race) send no type.
	default:
		return fmt.Errorf("content type %q not accepted", ct)
	}
	return nil
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func roleOf(r *http.Request) string {
	role, _ := r.Context().Value(ctxKey{}).(string)
	return role
}

// clientKey identifies the client for login rate limiting: the TCP peer
// address, with IPv6 grouped by /64 (one host can hold a whole /64).
// Proxy headers aren't trusted: the app is served directly.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.To4() != nil {
		return host
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token, role string) {
	maxAge := auth.VolunteerTTL
	if role == store.RoleAdmin {
		maxAge = auth.AdminMaxAge
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(maxAge.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
}

// securityHeaders sets conservative headers on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; style-src 'self'; script-src 'self'; "+
			"object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("web: handler panicked", "path", r.URL.Path, "panic", v)
				writeError(w, r, s.log, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// decodeJSON reads a JSON body strictly (unknown fields rejected).
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &httpError{http.StatusRequestEntityTooLarge, "too_large", "request body too large"}
		}
		return &httpError{http.StatusBadRequest, "bad_json", "invalid JSON: " + err.Error()}
	}
	if dec.More() {
		return &httpError{http.StatusBadRequest, "bad_json", "invalid JSON: trailing data"}
	}
	return nil
}

// uploadMemory is how much of an upload is held in RAM; the rest spills
// to a temp file (a Pi Zero has 512 MB).
const uploadMemory = 1 << 20

// uploadedFile returns the "file" part of a multipart upload.
func uploadedFile(r *http.Request) (io.ReadCloser, error) {
	if err := r.ParseMultipartForm(uploadMemory); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &httpError{http.StatusRequestEntityTooLarge, "too_large", "file too large"}
		}
		return nil, &httpError{http.StatusBadRequest, "no_file", "upload a file in the \"file\" field"}
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return nil, &httpError{http.StatusRequestEntityTooLarge, "too_large", "file too large"}
		}
		return nil, &httpError{http.StatusBadRequest, "no_file", "upload a file in the \"file\" field"}
	}
	return f, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
