package inbox

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"time"

	"checkin-board/internal/graywolf"
)

// fakeGW mimics graywolf's message feed: rows ordered by an update
// sequence (graywolf orders by updated_at, then id), paged by an opaque
// cursor, with an SSE stream the test drives by hand.
type fakeGW struct {
	mu       sync.Mutex
	rows     map[uint64]*fakeRow
	seq      int
	pageSize int
	reads    []uint64
	readErr  error
	listErr  error
	lists    int
	maxPages int // >0: stop after this many pages with ErrCatchUpIncomplete
	sinces   []time.Time

	events    chan graywolf.Event
	streams   chan struct{} // receives once per StreamEvents call
	streamErr error
}

type fakeRow struct {
	msg       graywolf.Message
	updateSeq int
}

func newFakeGW() *fakeGW {
	return &fakeGW{
		rows:     map[uint64]*fakeRow{},
		pageSize: 2,
		events:   make(chan graywolf.Event, 16),
		streams:  make(chan struct{}, 16),
	}
}

// put inserts or updates a row, moving it to the end of the feed.
func (f *fakeGW) put(m graywolf.Message) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	f.rows[m.ID] = &fakeRow{msg: m, updateSeq: f.seq}
}

func (f *fakeGW) inbound(id uint64, text string) graywolf.Message {
	m := graywolf.Message{ID: id, Direction: "in", ThreadKind: graywolf.ThreadKindDM, FromCall: "K1CP", Text: text, Status: graywolf.StatusReceived}
	f.put(m)
	return m
}

func (f *fakeGW) readIDs() []uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reads)
}

func (f *fakeGW) listCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lists
}

func (f *fakeGW) CatchUp(ctx context.Context, p graywolf.ListParams, fn func(graywolf.MessageChange) error) (string, error) {
	f.mu.Lock()
	f.lists++
	if f.listErr != nil {
		err := f.listErr
		f.mu.Unlock()
		return p.Cursor, err
	}
	if p.Cursor == "" {
		f.sinces = append(f.sinces, p.Since)
	}
	after := 0
	if p.Cursor != "" {
		after, _ = strconv.Atoi(p.Cursor)
	}
	var feed []*fakeRow
	for _, r := range f.rows {
		created := time.Unix(int64(r.msg.ID), 0)
		if r.updateSeq > after && (p.Since.IsZero() || !created.Before(p.Since)) {
			copied := *r
			feed = append(feed, &copied)
		}
	}
	f.mu.Unlock()
	slices.SortFunc(feed, func(a, b *fakeRow) int { return a.updateSeq - b.updateSeq })

	cursor := p.Cursor
	pages := 0
	for len(feed) > 0 {
		if f.maxPages > 0 && pages == f.maxPages {
			return cursor, graywolf.ErrCatchUpIncomplete
		}
		pages++
		n := min(f.pageSize, len(feed))
		page := feed[:n]
		feed = feed[n:]
		for _, r := range page {
			m := r.msg
			if err := fn(graywolf.MessageChange{ID: m.ID, Kind: "updated", Message: &m}); err != nil {
				return cursor, err
			}
		}
		cursor = strconv.Itoa(page[len(page)-1].updateSeq)
	}
	return cursor, nil
}

func (f *fakeGW) MarkRead(ctx context.Context, id uint64) error {
	f.mu.Lock()
	err := f.readErr
	f.reads = append(f.reads, id)
	r, ok := f.rows[id]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if ok && r.msg.Unread {
		m := r.msg
		m.Unread = false
		f.put(m) // like graywolf: marking read bumps updated_at
	}
	return nil
}

func (f *fakeGW) StreamEventsWithOpen(ctx context.Context, onOpen func(), fn func(graywolf.Event) error) error {
	f.mu.Lock()
	err, events := f.streamErr, f.events
	f.mu.Unlock()
	f.streams <- struct{}{}
	if err != nil {
		return err
	}
	onOpen()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-events:
			if !ok {
				return graywolf.ErrStreamClosed
			}
			if err := fn(ev); err != nil {
				return err
			}
		}
	}
}

// recorder is a Dispatcher that records what it was given.
type recorder struct {
	mu       sync.Mutex
	inbound  []uint64
	outbound []string       // "id:status"
	failIn   map[uint64]int // id -> remaining failures
	permIn   map[uint64]bool
	got      chan struct{}
}

func newRecorder() *recorder {
	return &recorder{failIn: map[uint64]int{}, permIn: map[uint64]bool{}, got: make(chan struct{}, 64)}
}

var errDispatch = errors.New("dispatch failed")

func (r *recorder) HandleInbound(ctx context.Context, m graywolf.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.permIn[m.ID] {
		return Permanent(errDispatch)
	}
	if r.failIn[m.ID] > 0 {
		r.failIn[m.ID]--
		return errDispatch
	}
	r.inbound = append(r.inbound, m.ID)
	select {
	case r.got <- struct{}{}:
	default:
	}
	return nil
}

func (r *recorder) HandleOutbound(ctx context.Context, m graywolf.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outbound = append(r.outbound, strconv.FormatUint(m.ID, 10)+":"+m.Status)
	r.got <- struct{}{}
	return nil
}

func (r *recorder) inboundIDs() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.inbound)
}

func (r *recorder) outboundSeen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.outbound)
}
