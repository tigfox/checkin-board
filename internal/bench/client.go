package bench

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"strconv"
	"strings"
	"time"
)

const (
	requestTimeout = 15 * time.Second
	maxBody        = 8 << 20 // results export of a large race
)

// Node is one checkin-board node's web API, logged in as admin (an
// admin session also works the keypad).
type Node struct {
	Base string
	http *http.Client
}

// NewNode returns a client for base, e.g. "http://10.0.1.183:8090".
func NewNode(base string) *Node {
	jar, _ := cookiejar.New(nil)
	return &Node{Base: strings.TrimRight(base, "/"), http: &http.Client{Jar: jar, Timeout: requestTimeout}}
}

// Station is the node's role and state (GET /api/station).
type Station struct {
	Role       string   `json:"role"`
	RaceState  string   `json:"race_state"`
	LocalCodes []string `json:"local_codes"`
}

// APIError is a non-2xx reply.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// Retryable reports whether a request may be sent again with the same
// request_id: a network error, a server-side failure, 409 (the node is
// still saving the first attempt) or 429 (rate limited).
func Retryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500 || ae.Status == http.StatusConflict || ae.Status == http.StatusTooManyRequests
	}
	return err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func (n *Node) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.Base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Origin", n.Base) // the app's same-origin check
	resp, err := n.http.Do(req)
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
	if out == nil {
		return nil
	}
	if raw, ok := out.(*[]byte); ok {
		*raw = data
		return nil
	}
	return json.Unmarshal(data, out)
}

// Login logs in as admin.
func (n *Node) Login(ctx context.Context, password string) error {
	return n.do(ctx, http.MethodPost, "/api/login", map[string]string{"role": "admin", "password": password}, nil)
}

// Station reads the node's role and state.
func (n *Node) Station(ctx context.Context) (Station, error) {
	var s Station
	err := n.do(ctx, http.MethodGet, "/api/station", nil, &s)
	return s, err
}

// SyncClock sets the node's race clock from this machine's clock, as the
// keypad page does from the browser's.
func (n *Node) SyncClock(ctx context.Context, now time.Time) error {
	return n.do(ctx, http.MethodPost, "/api/clock/sync", map[string]int64{"client_unix_ms": now.UnixMilli()}, nil)
}

// LogBib logs bib on the node's keypad. requestID makes a retry safe:
// the node answers a repeat with the first result.
func (n *Node) LogBib(ctx context.Context, bib int, requestID string) (time.Time, error) {
	var res struct {
		TimeIn time.Time `json:"time_in"`
	}
	err := n.do(ctx, http.MethodPost, "/api/entries", map[string]string{"bib": strconv.Itoa(bib), "request_id": requestID}, &res)
	if err == nil && res.TimeIn.IsZero() {
		err = errors.New("bench: the node's reply has no time_in")
	}
	return res.TimeIn, err
}

// HQCall reads the HQ callsign a checkpoint reports to.
func (n *Node) HQCall(ctx context.Context) (string, error) {
	var s struct {
		HQCall string `json:"hq_call"`
	}
	err := n.do(ctx, http.MethodGet, "/api/admin/settings", nil, &s)
	return s.HQCall, err
}

// RecentBibs lists the bibs of the node's latest keypad entries (up to
// 500, the API's limit).
func (n *Node) RecentBibs(ctx context.Context) ([]int, error) {
	var res struct {
		Entries []struct{ Bib int } `json:"entries"`
	}
	if err := n.do(ctx, http.MethodGet, "/api/entries?limit=500", nil, &res); err != nil {
		return nil, err
	}
	bibs := make([]int, len(res.Entries))
	for i, e := range res.Entries {
		bibs[i] = e.Bib
	}
	return bibs, nil
}

// Results reads HQ's results export.
func (n *Node) Results(ctx context.Context) ([]Result, error) {
	var raw []byte
	if err := n.do(ctx, http.MethodGet, "/api/admin/export.csv", nil, &raw); err != nil {
		return nil, err
	}
	return ParseResults(raw)
}

// ParseResults reads HQ's results CSV (bib, category, checkpoint,
// checkpoint_name, time_in, source, received_at, count).
func ParseResults(data []byte) ([]Result, error) {
	rows, err := csv.NewReader(bytes.NewReader(data)).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("bench: results CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, errors.New("bench: results CSV is empty")
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[h] = i
	}
	for _, h := range []string{"bib", "checkpoint", "time_in", "received_at", "count"} {
		if _, ok := col[h]; !ok {
			return nil, fmt.Errorf("bench: results CSV has no %q column", h)
		}
	}
	out := make([]Result, 0, len(rows)-1)
	for i, row := range rows[1:] {
		bib, err1 := strconv.Atoi(row[col["bib"]])
		in, err2 := time.Parse(time.RFC3339, row[col["time_in"]])
		recv, err3 := time.Parse(time.RFC3339, row[col["received_at"]])
		count, err4 := strconv.Atoi(row[col["count"]])
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			return nil, fmt.Errorf("bench: results CSV row %d: %w", i+2, err)
		}
		out = append(out, Result{Code: row[col["checkpoint"]], Bib: bib, TimeIn: in, ReceivedAt: recv, Count: count})
	}
	return out, nil
}
