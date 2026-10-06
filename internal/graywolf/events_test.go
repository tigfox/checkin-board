package graywolf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func sseHandler(t *testing.T, frames ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		_, _ = fmt.Fprint(w, ": connected\n\n")
		fl.Flush()
		for _, f := range frames {
			_, _ = fmt.Fprint(w, f)
			fl.Flush()
		}
	}
}

func TestStreamEventsParsesFrames(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t,
		": keepalive\n\n",
		"event: message.received\ndata: {\"id\":5,\"kind\":\"created\",\"message\":{\"id\":5,\"text\":\"RC1 H 3 0 120000\"}}\n\n",
		"event: message.deleted\r\ndata: {\"id\":6,\"kind\":\"deleted\"}\r\n\r\n",
		"data: {\"id\":7,\"kind\":\"updated\"}\n\n", // no event: line
	))
	var got []Event
	err := f.client(t).StreamEvents(context.Background(), func(e Event) error {
		got = append(got, e)
		return nil
	})
	if !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("err = %v, want ErrStreamClosed", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %+v", got)
	}
	if got[0].Type != EventReceived || got[0].Change.Message == nil || got[0].Change.Message.Text != "RC1 H 3 0 120000" {
		t.Errorf("event 0 = %+v", got[0])
	}
	if got[1].Type != EventDeleted || got[1].Change.ID != 6 || got[1].Change.Message != nil {
		t.Errorf("event 1 = %+v", got[1])
	}
	if got[2].Type != "message" || got[2].Change.ID != 7 {
		t.Errorf("event 2 = %+v", got[2])
	}
}

func TestStreamEventsMultiLineData(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t,
		"event: message.acked\ndata: {\"id\":1,\ndata: \"kind\":\"updated\"}\n\n",
	))
	var got []Event
	_ = f.client(t).StreamEvents(context.Background(), func(e Event) error {
		got = append(got, e)
		return nil
	})
	if len(got) != 1 || got[0].Change.ID != 1 || got[0].Change.Kind != "updated" {
		t.Fatalf("got %+v", got)
	}
}

func TestStreamEventsSkipsBadJSON(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t,
		"event: message.acked\ndata: {broken\n\n",
		"event: message.acked\ndata: {\"id\":2}\n\n",
	))
	var ids []uint64
	_ = f.client(t).StreamEvents(context.Background(), func(e Event) error {
		ids = append(ids, e.Change.ID)
		return nil
	})
	if len(ids) != 1 || ids[0] != 2 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestStreamEventsHandlerErrorStops(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t,
		"event: message.acked\ndata: {\"id\":1}\n\n",
		"event: message.acked\ndata: {\"id\":2}\n\n",
	))
	stop := errors.New("stop")
	n := 0
	err := f.client(t).StreamEvents(context.Background(), func(e Event) error {
		n++
		return stop
	})
	if !errors.Is(err, stop) || n != 1 {
		t.Fatalf("err = %v, n = %d", err, n)
	}
}

func TestStreamEventsReloginOn401(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t, "event: message.acked\ndata: {\"id\":1}\n\n"))
	c := f.client(t)
	// Establish then expire a session so the stream's first attempt 401s.
	f.mux.HandleFunc("GET /api/version", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, Version{})
	})
	if _, err := c.Version(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.expireSession()

	n := 0
	err := c.StreamEvents(context.Background(), func(Event) error { n++; return nil })
	if !errors.Is(err, ErrStreamClosed) || n != 1 {
		t.Fatalf("err = %v, n = %d", err, n)
	}
	if f.logins.Load() != 2 {
		t.Errorf("logins = %d, want 2", f.logins.Load())
	}
}

func TestStreamEventsNonOKStatus(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "messages disabled"})
	})
	err := f.client(t).StreamEvents(context.Background(), func(Event) error { return nil })
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("got %v", err)
	}
}

func TestStreamEventsIdleTimeout(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, ": connected\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // then go silent
	})
	c := f.client(t)
	c.streamIdle = 100 * time.Millisecond

	start := time.Now()
	err := c.StreamEvents(context.Background(), func(Event) error { return nil })
	if !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("took %v", time.Since(start))
	}
}

func TestStreamEventsIdleBeforeHeaders(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never send headers
	})
	c := f.client(t)
	c.streamIdle = 100 * time.Millisecond
	if err := c.StreamEvents(context.Background(), func(Event) error { return nil }); !errors.Is(err, ErrStreamIdle) {
		t.Fatalf("err = %v, want ErrStreamIdle", err)
	}
}

func TestStreamEventsContextCancel(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := f.client(t).StreamEvents(ctx, func(Event) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestStreamEventsOversizedLine(t *testing.T) {
	f := newFakeGW(t)
	huge := "data: " + strings.Repeat("x", maxSSELine+10) + "\n\n"
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t, huge))
	err := f.client(t).StreamEvents(context.Background(), func(Event) error { return nil })
	if err == nil || errors.Is(err, ErrStreamClosed) {
		t.Fatalf("want scan error, got %v", err)
	}
}

func TestStreamEventsSlowHandlerIsNotIdle(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages/events", sseHandler(t,
		"event: message.acked\ndata: {\"id\":1}\n\n",
		"event: message.acked\ndata: {\"id\":2}\n\n",
	))
	c := f.client(t)
	c.streamIdle = 100 * time.Millisecond
	n := 0
	err := c.StreamEvents(context.Background(), func(Event) error {
		n++
		time.Sleep(250 * time.Millisecond) // longer than the idle window
		return nil
	})
	if !errors.Is(err, ErrStreamClosed) || n != 2 {
		t.Fatalf("err = %v, n = %d; want ErrStreamClosed after 2 events", err, n)
	}
}

func TestStreamEventsWithOpenCallsOnOpenOnlyOnSuccess(t *testing.T) {
	f := newFakeGW(t)
	ok := true
	f.mux.HandleFunc("GET /api/messages/events", func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			writeTestJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "down"})
			return
		}
		sseHandler(t, "event: message.acked\ndata: {\"id\":1}\n\n")(w, r)
	})
	c := f.client(t)
	var order []string
	err := c.StreamEventsWithOpen(context.Background(),
		func() { order = append(order, "open") },
		func(Event) error { order = append(order, "event"); return nil })
	if !errors.Is(err, ErrStreamClosed) || strings.Join(order, ",") != "open,event" {
		t.Fatalf("err = %v, order = %v", err, order)
	}
	ok = false
	opened := false
	_ = c.StreamEventsWithOpen(context.Background(), func() { opened = true }, func(Event) error { return nil })
	if opened {
		t.Fatal("onOpen called for a refused stream")
	}
}
