// Package graywolf is a typed client for the subset of graywolf's
// published REST API that checkin-board uses (messages, station,
// health, version). It owns the session: it logs in lazily, re-logs in
// once on a 401, and never exposes or logs the password.
package graywolf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	sessionCookieName = "graywolf_session"
	loginPath         = "/api/auth/login"

	defaultTimeout    = 10 * time.Second
	defaultStreamIdle = 45 * time.Second // graywolf keepalives every 15 s
	defaultAuthRetry  = 30 * time.Second // after bad credentials, fail fast this long

	maxResponseBody = 4 << 20
	maxErrorBody    = 512
)

// ErrAuth means graywolf rejected our credentials or session.
var ErrAuth = errors.New("graywolf: authentication failed")

// APIError is a non-2xx response from graywolf.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("graywolf: %s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a graywolf 404.
func IsNotFound(err error) bool { return hasStatus(err, http.StatusNotFound) }

// IsConflict reports whether err is a graywolf 409 (e.g. resend already in flight).
func IsConflict(err error) bool { return hasStatus(err, http.StatusConflict) }

func hasStatus(err error, code int) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == code
}

// Config configures a Client.
type Config struct {
	BaseURL  string
	Username string
	Password string
	// Timeout bounds each non-streaming request. Zero means 10 s.
	Timeout time.Duration
	// Logger receives diagnostics. Nil discards them.
	Logger *slog.Logger
}

// String never includes the password.
func (c Config) String() string {
	return fmt.Sprintf("graywolf.Config{BaseURL:%s Username:%s Timeout:%s}", c.BaseURL, c.Username, c.Timeout)
}

// GoString keeps %#v from printing the password.
func (c Config) GoString() string { return c.String() }

// LogValue keeps slog from printing the password.
func (c Config) LogValue() slog.Value { return slog.StringValue(c.String()) }

// Client talks to one graywolf instance. It is safe for concurrent use.
type Client struct {
	base       *url.URL
	username   string
	password   string
	http       *http.Client
	stream     *http.Client
	streamIdle time.Duration
	authRetry  time.Duration
	logger     *slog.Logger

	mu           sync.Mutex
	session      string
	gen          uint64 // bumped on every successful login
	authFailedAt time.Time
}

// String never includes the password.
func (c *Client) String() string {
	return fmt.Sprintf("graywolf.Client{%s user=%s}", c.base.Redacted(), c.username)
}

// New validates cfg and returns a Client. It does not contact graywolf.
func New(cfg Config) (*Client, error) {
	base, err := parseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("graywolf: username and password are required")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Client{
		base:       base,
		username:   cfg.Username,
		password:   cfg.Password,
		http:       &http.Client{Timeout: timeout},
		stream:     &http.Client{}, // bounded by ctx and the idle watchdog instead
		streamIdle: defaultStreamIdle,
		authRetry:  defaultAuthRetry,
		logger:     logger,
	}, nil
}

func parseBaseURL(raw string) (*url.URL, error) {
	// Errors never echo raw: it might embed credentials.
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errors.New("graywolf: base url is not a valid URL")
	}
	if u.User != nil {
		return nil, errors.New("graywolf: base url must not contain credentials; use Username/Password")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("graywolf: base url %s must be http or https", u.Redacted())
	}
	if u.Host == "" {
		return nil, fmt.Errorf("graywolf: base url %s has no host", u.Redacted())
	}
	return u, nil
}

// Login forces a fresh session, ignoring any auth backoff. Normal
// callers never need it: every request logs in on demand.
func (c *Client) Login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.authFailedAt = time.Time{}
	return c.loginLocked(ctx)
}

func (c *Client) sessionState() (string, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session, c.gen
}

// refreshSession logs in unless another goroutine already replaced the
// session that failed (staleGen), so a burst of 401s logs in once.
func (c *Client) refreshSession(ctx context.Context, staleGen uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != "" && c.gen != staleGen {
		return nil
	}
	// After graywolf rejected our credentials, fail fast for a while
	// instead of logging in on every request (lockouts, log spam).
	if !c.authFailedAt.IsZero() && time.Since(c.authFailedAt) < c.authRetry {
		return ErrAuth
	}
	return c.loginLocked(ctx)
}

func (c *Client) loginLocked(ctx context.Context) error {
	body, err := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	if err != nil {
		return fmt.Errorf("graywolf: encode login: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url(loginPath, nil), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("graywolf: build login: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("graywolf: login: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		c.session = ""
		c.authFailedAt = time.Now()
		c.logger.Warn("graywolf rejected credentials", "user", c.username)
		return ErrAuth
	}
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp, http.MethodPost, loginPath)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookieName && ck.Value != "" {
			c.session = ck.Value
			c.gen++
			c.authFailedAt = time.Time{}
			c.logger.Debug("graywolf login ok")
			return nil
		}
	}
	return fmt.Errorf("graywolf: login succeeded but no %s cookie was set", sessionCookieName)
}

func (c *Client) url(path string, query url.Values) string {
	u := c.base.JoinPath(path)
	u.RawQuery = query.Encode()
	return u.String()
}

// do sends a JSON request and decodes a JSON response into out (if
// non-nil). It logs in when needed and retries once after a 401.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	var body []byte
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("graywolf: encode %s %s: %w", method, path, err)
		}
		body = b
	}
	resp, err := c.send(ctx, c.http, method, path, query, body, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return readAPIError(resp, method, path)
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return fmt.Errorf("graywolf: decode %s %s: %w", method, path, err)
	}
	return nil
}

// send performs one authenticated round trip, re-logging in once on a
// 401. The caller owns the returned body.
func (c *Client) send(ctx context.Context, hc *http.Client, method, path string, query url.Values, body []byte, accept string) (*http.Response, error) {
	session, gen := c.sessionState()
	if session == "" {
		if err := c.refreshSession(ctx, gen); err != nil {
			return nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		session, gen = c.sessionState()
		resp, err := c.roundTrip(ctx, hc, method, path, query, body, accept, session)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized {
			return resp, nil
		}
		resp.Body.Close()
		if attempt > 0 {
			return nil, fmt.Errorf("%w: %s %s still unauthorized after re-login", ErrAuth, method, path)
		}
		if err := c.refreshSession(ctx, gen); err != nil {
			return nil, err
		}
	}
}

func (c *Client) roundTrip(ctx context.Context, hc *http.Client, method, path string, query url.Values, body []byte, accept, session string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path, query), rdr)
	if err != nil {
		return nil, fmt.Errorf("graywolf: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Set the cookie by hand rather than through a jar, so cookie
	// attributes (Secure on a plain-http localhost) can't drop it.
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("graywolf: %s %s: %w", method, path, err)
	}
	return resp, nil
}

func readAPIError(resp *http.Response, method, path string) error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	msg := strings.TrimSpace(string(raw))
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Error != "" {
		msg = envelope.Error
	}
	return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: msg}
}
