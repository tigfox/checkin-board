// Package nodeapi is a small client for a checkin-board node's own web
// API, as an operator's browser uses it: an admin login, then JSON calls.
// The bench (benchrace) and station setup (stationsetup) tools use it.
package nodeapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"strings"
	"time"
)

const (
	// DefaultPort is the app's web port (CB_LISTEN's default).
	DefaultPort = "8090"
	timeout     = 30 * time.Second
	maxBody     = 8 << 20
)

// Client talks to one node, keeping its session cookie.
type Client struct {
	Base string
	http *http.Client
}

// New returns a client for addr: a base URL ("http://10.0.1.183:8090")
// or a bare host or IP, which gets http:// and port 8090.
func New(addr string) *Client {
	jar, _ := cookiejar.New(nil)
	// No redirects: a login body (the password) must only ever go to the
	// address the operator gave.
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{Base: BaseURL(addr), http: &http.Client{Jar: jar, Timeout: timeout, CheckRedirect: noRedirect}}
}

// BaseURL normalizes a node address to a base URL.
func BaseURL(addr string) string {
	addr = strings.TrimRight(strings.TrimSpace(addr), "/")
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
		if host := strings.TrimPrefix(addr, "http://"); !strings.Contains(host, ":") || strings.HasSuffix(host, "]") {
			addr += ":" + DefaultPort
		}
	}
	return addr
}

// APIError is a non-2xx reply.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	var b struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &b) == nil && b.Error != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, b.Error)
	}
	return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body)
}

// StatusOf is err's HTTP status (0: not an API error).
func StatusOf(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// Do sends a JSON request (body nil: none) and decodes a JSON reply into
// out (nil: ignored; *[]byte: raw).
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	return c.send(ctx, method, path, rd, "application/json", body != nil, out)
}

// Upload posts a file as the multipart field "file", with extra form
// fields.
func (c *Client) Upload(ctx context.Context, path, name string, data []byte, fields map[string]string, out any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	fw, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	return c.send(ctx, http.MethodPost, path, &buf, mw.FormDataContentType(), true, out)
}

func (c *Client) send(ctx context.Context, method, path string, body io.Reader, ctype string, hasBody bool, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	if hasBody {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Origin", c.Base) // the app's same-origin check
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: strings.TrimSpace(string(data))}
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *[]byte:
		*o = data
		return nil
	default:
		return json.Unmarshal(data, out)
	}
}

// Login logs in as admin.
func (c *Client) Login(ctx context.Context, password string) error {
	return c.Do(ctx, http.MethodPost, "/api/login", map[string]string{"role": "admin", "password": password}, nil)
}
