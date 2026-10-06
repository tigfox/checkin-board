package graywolf

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

const (
	eventsPath       = "/api/messages/events"
	defaultEventType = "message"
	maxSSELine       = 1 << 20
)

var (
	// ErrStreamClosed means graywolf ended the event stream normally.
	ErrStreamClosed = errors.New("graywolf: event stream closed")
	// ErrStreamIdle means no bytes (not even a keepalive) arrived in time.
	ErrStreamIdle = errors.New("graywolf: event stream idle")
)

// StreamEvents holds one connection to /api/messages/events and calls fn
// for each event until the stream ends, ctx is done, or fn returns an
// error. It always returns a non-nil error; reconnecting is the
// caller's job. Events are hints: graywolf can drop them, so callers
// must also page with CatchUp.
func (c *Client) StreamEvents(ctx context.Context, fn func(Event) error) error {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The watchdog covers connecting too: a graywolf that accepts the
	// connection but never answers must not hang the reader.
	var idle atomic.Bool
	watchdog := time.AfterFunc(c.streamIdle, func() {
		idle.Store(true)
		cancel()
	})
	defer watchdog.Stop()

	resp, err := c.send(streamCtx, c.stream, http.MethodGet, eventsPath, nil, nil, "text/event-stream")
	if err != nil {
		if idle.Load() {
			return ErrStreamIdle
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return readAPIError(resp, http.MethodGet, eventsPath)
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 4096), maxSSELine)
	p := sseParser{onEvent: c.decodeEvent(fn)}
	for scanner.Scan() {
		// Pause the watchdog while fn runs: a slow handler is not an
		// idle server.
		watchdog.Stop()
		if err := p.line(scanner.Text()); err != nil {
			return err
		}
		watchdog.Reset(c.streamIdle)
	}

	switch {
	case idle.Load():
		return ErrStreamIdle
	case ctx.Err() != nil:
		return ctx.Err()
	case scanner.Err() != nil:
		return fmt.Errorf("graywolf: read event stream: %w", scanner.Err())
	default:
		return ErrStreamClosed
	}
}

// decodeEvent turns a raw SSE frame into an Event. A frame with bad
// JSON is logged and skipped rather than ending the stream.
func (c *Client) decodeEvent(fn func(Event) error) func(string, string) error {
	return func(typ, data string) error {
		var change MessageChange
		if err := json.Unmarshal([]byte(data), &change); err != nil {
			c.logger.Warn("graywolf: skipping undecodable event", "type", typ, "err", err)
			return nil
		}
		return fn(Event{Type: typ, Change: change})
	}
}

// sseParser implements the subset of the SSE line protocol graywolf
// uses: comments, event:, data: (possibly multi-line), blank-line dispatch.
type sseParser struct {
	onEvent func(typ, data string) error
	typ     string
	data    []string
}

func (p *sseParser) line(l string) error {
	if l == "" {
		return p.dispatch()
	}
	if strings.HasPrefix(l, ":") {
		return nil
	}
	field, value, _ := strings.Cut(l, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		p.typ = value
	case "data":
		p.data = append(p.data, value)
	}
	return nil
}

func (p *sseParser) dispatch() error {
	typ, data := p.typ, p.data
	p.typ, p.data = "", nil
	if len(data) == 0 {
		return nil
	}
	if typ == "" {
		typ = defaultEventType
	}
	return p.onEvent(typ, strings.Join(data, "\n"))
}
