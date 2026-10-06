// Package apiclient is a thin JSON-over-HTTP client for the local API.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxErrorBody caps how much of a failed response body is kept in an APIError.
const maxErrorBody = 4 << 10

// APIError is returned when the API responds with a non-2xx status.
type APIError struct {
	StatusCode int
	Body       string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api returned %d: %s", e.StatusCode, e.Body)
}

// Client talks to the local API.
type Client struct {
	baseURL *url.URL
	http    *http.Client
}

// Option customizes a Client.
type Option func(*Client)

// WithHTTPClient overrides the underlying *http.Client.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.http = hc }
}

// New builds a Client for baseURL with the given request timeout.
func New(baseURL string, timeout time.Duration, opts ...Option) (*Client, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parse base url: %w", err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("base url %q must include scheme and host", baseURL)
	}

	c := &Client{
		baseURL: u,
		http:    &http.Client{Timeout: timeout},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c, nil
}

// HealthStatus is the expected payload of GET /health.
// TODO: adjust to match the real API.
type HealthStatus struct {
	Status string `json:"status"`
}

// Health calls GET /health.
func (c *Client) Health(ctx context.Context) (HealthStatus, error) {
	var out HealthStatus
	if err := c.Get(ctx, "/health", &out); err != nil {
		return HealthStatus{}, fmt.Errorf("health check: %w", err)
	}
	return out, nil
}

// Get issues a GET to path and decodes the JSON response into out.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

// Post sends in as JSON to path and decodes the JSON response into out.
// Either in or out may be nil.
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	return c.do(ctx, http.MethodPost, path, in, out)
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	req, err := c.newRequest(ctx, method, path, in)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return &APIError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	rel, err := url.Parse(strings.TrimPrefix(path, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse path %q: %w", path, err)
	}
	target := c.baseURL.JoinPath(rel.Path)
	target.RawQuery = rel.RawQuery

	var body io.Reader
	if in != nil {
		buf, err := json.Marshal(in)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		body = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}
