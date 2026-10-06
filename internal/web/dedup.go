package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"
)

// Keypad retry protection: a POST /api/entries carrying a request_id is
// answered once; a retry with the same id gets the stored response.
const (
	maxRequestIDLen = 64
	dedupTTL        = 10 * time.Minute
	dedupMax        = 4096
)

type dedupEntry struct {
	key     string // what the request asked for; a retry must match it
	done    bool
	status  int
	result  json.RawMessage
	expires time.Time
}

type requestDedup struct {
	now func() time.Time

	mu      sync.Mutex
	entries map[string]*dedupEntry
}

func newRequestDedup(now func() time.Time) *requestDedup {
	return &requestDedup{now: now, entries: map[string]*dedupEntry{}}
}

var (
	inProgressBody = json.RawMessage(`{"error":"this entry is still being saved; try again","code":"in_progress"}`)
	mismatchBody   = json.RawMessage(`{"error":"request_id was already used for a different entry","code":"request_id_reused"}`)
)

// begin claims id for a request whose content is key (e.g. bib|cp). If
// it's new (ok), the caller handles the request and calls release with
// the response, which is kept only if it succeeded (2xx): a failed
// attempt can be retried for real. If id was already answered, the
// stored response is returned; if it is still in flight, the retry is
// told to wait; if it was used for a different key, it's refused.
func (d *requestDedup) begin(id, key string) (release func(int, []byte), prior struct {
	status int
	result json.RawMessage
}, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if e := d.entries[id]; e != nil && now.Before(e.expires) {
		switch {
		case e.key != key:
			prior.status, prior.result = http.StatusUnprocessableEntity, mismatchBody
		case e.done:
			prior.status, prior.result = e.status, e.result
		default:
			prior.status, prior.result = http.StatusConflict, inProgressBody
		}
		return nil, prior, false
	}
	if len(d.entries) >= dedupMax {
		d.evict(now)
	}
	e := &dedupEntry{key: key, expires: now.Add(dedupTTL)}
	d.entries[id] = e
	return func(status int, body []byte) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.entries[id] != e {
			return // expired and replaced (or evicted) meanwhile
		}
		if status < 200 || status > 299 {
			delete(d.entries, id)
			return
		}
		e.done, e.status, e.result = true, status, json.RawMessage(bytes.TrimSpace(body))
	}, prior, true
}

// evict drops expired entries, then the oldest answered ones until a
// quarter of the cap is free, so a full cache isn't rescanned on every
// request. In-flight claims are never dropped. Called with d.mu held.
func (d *requestDedup) evict(now time.Time) {
	type aged struct {
		id      string
		expires time.Time
	}
	var done []aged
	for id, e := range d.entries {
		switch {
		case !now.Before(e.expires):
			delete(d.entries, id)
		case e.done:
			done = append(done, aged{id, e.expires})
		}
	}
	excess := len(d.entries) - dedupMax*3/4
	if excess <= 0 {
		return
	}
	slices.SortFunc(done, func(a, b aged) int { return a.expires.Compare(b.expires) })
	for _, a := range done[:min(excess, len(done))] {
		delete(d.entries, a.id)
	}
}

// recordingWriter keeps a copy of the response for the dedup cache.
type recordingWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *recordingWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (w *recordingWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.body.Write(b)
	return w.ResponseWriter.Write(b)
}
