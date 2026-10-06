package graywolf

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"
	"time"
)

func TestSendMessage(t *testing.T) {
	f := newFakeGW(t)
	var got SendRequest
	f.mux.HandleFunc("POST /api/messages", func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeTestJSON(w, http.StatusAccepted, Message{ID: 42, MsgID: "17", Status: StatusQueued, ClientID: got.ClientID})
	})
	c := f.client(t)
	ch := 2
	msg, err := c.SendMessage(context.Background(), SendRequest{
		To: "hq-1", Text: "RC1 R 3 1 @1200 101/05", Path: "WIDE2-1", Channel: &ch, ClientID: "3-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if msg.ID != 42 || msg.MsgID != "17" {
		t.Errorf("msg = %+v", msg)
	}
	if got.To != "HQ-1" || got.Path != "WIDE2-1" || got.Channel == nil || *got.Channel != 2 || got.ClientID != "3-1" {
		t.Errorf("request = %+v", got)
	}
}

func TestSendMessageValidates(t *testing.T) {
	c := newFakeGW(t).client(t)
	long := make([]byte, MaxMessageTextUnsafe+1)
	for i := range long {
		long[i] = 'x'
	}
	tests := []SendRequest{
		{To: "", Text: "hi"},
		{To: "BAD CALL", Text: "hi"},
		{To: "HQ", Text: ""},
		{To: "HQ", Text: string(long)},
		{To: "HQ", Text: "line\nbreak"},
	}
	for _, req := range tests {
		if _, err := c.SendMessage(context.Background(), req); err == nil {
			t.Errorf("SendMessage(%+v): expected error", req)
		}
	}
}

func TestGetResendDeleteMarkRead(t *testing.T) {
	f := newFakeGW(t)
	calls := map[string]int{}
	f.mux.HandleFunc("GET /api/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		calls["get"]++
		id, _ := strconv.ParseUint(r.PathValue("id"), 10, 64)
		writeTestJSON(w, http.StatusOK, Message{ID: id, Status: StatusAcked, MsgID: "5"})
	})
	f.mux.HandleFunc("POST /api/messages/{id}/resend", func(w http.ResponseWriter, r *http.Request) {
		calls["resend"]++
		if r.PathValue("id") == "9" {
			writeTestJSON(w, http.StatusConflict, map[string]string{"error": "resend already in flight"})
			return
		}
		writeTestJSON(w, http.StatusAccepted, Message{ID: 1, MsgID: "5", Status: StatusTxSubmitted})
	})
	f.mux.HandleFunc("DELETE /api/messages/{id}", func(w http.ResponseWriter, r *http.Request) {
		calls["delete"]++
		w.WriteHeader(http.StatusNoContent)
	})
	f.mux.HandleFunc("POST /api/messages/{id}/read", func(w http.ResponseWriter, r *http.Request) {
		calls["read"]++
		w.WriteHeader(http.StatusNoContent)
	})
	c := f.client(t)
	ctx := context.Background()

	m, err := c.GetMessage(ctx, 3)
	if err != nil || m.ID != 3 || m.Status != StatusAcked {
		t.Fatalf("get: %+v %v", m, err)
	}
	m, err = c.ResendMessage(ctx, 1)
	if err != nil || m.MsgID != "5" {
		t.Fatalf("resend: %+v %v", m, err)
	}
	if _, err := c.ResendMessage(ctx, 9); !IsConflict(err) {
		t.Fatalf("resend in flight: want conflict, got %v", err)
	}
	if err := c.DeleteMessage(ctx, 1); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.MarkRead(ctx, 1); err != nil {
		t.Fatalf("read: %v", err)
	}
	want := map[string]int{"get": 1, "resend": 2, "delete": 1, "read": 1}
	for k, v := range want {
		if calls[k] != v {
			t.Errorf("%s calls = %d, want %d", k, calls[k], v)
		}
	}
}

func TestZeroIDRejected(t *testing.T) {
	c := newFakeGW(t).client(t)
	ctx := context.Background()
	if _, err := c.GetMessage(ctx, 0); err == nil {
		t.Error("GetMessage(0): expected error")
	}
	if _, err := c.ResendMessage(ctx, 0); err == nil {
		t.Error("ResendMessage(0): expected error")
	}
	if err := c.DeleteMessage(ctx, 0); err == nil {
		t.Error("DeleteMessage(0): expected error")
	}
	if err := c.MarkRead(ctx, 0); err == nil {
		t.Error("MarkRead(0): expected error")
	}
}

func TestListMessagesEncodesParams(t *testing.T) {
	f := newFakeGW(t)
	since := time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)
	f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		checks := map[string]string{
			"folder": "inbox", "peer": "HQ", "thread_kind": "dm", "thread_key": "HQ",
			"since": "2026-10-05T06:00:00Z", "cursor": "abc", "unread_only": "true", "limit": "50",
		}
		for k, v := range checks {
			if q.Get(k) != v {
				t.Errorf("%s = %q, want %q", k, q.Get(k), v)
			}
		}
		writeTestJSON(w, http.StatusOK, MessagePage{Cursor: "def", Changes: []MessageChange{{ID: 1, Kind: "created", Message: &Message{ID: 1}}}})
	})
	page, err := f.client(t).ListMessages(context.Background(), ListParams{
		Folder: FolderInbox, Peer: "hq", ThreadKind: ThreadKindDM, ThreadKey: "hq",
		Since: since, Cursor: "abc", UnreadOnly: true, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.Cursor != "def" || len(page.Changes) != 1 {
		t.Errorf("page = %+v", page)
	}
}

func TestListMessagesOmitsZeroParams(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("query = %q, want empty", r.URL.RawQuery)
		}
		writeTestJSON(w, http.StatusOK, MessagePage{})
	})
	if _, err := f.client(t).ListMessages(context.Background(), ListParams{}); err != nil {
		t.Fatal(err)
	}
}

func TestListMessagesRejectsBadLimit(t *testing.T) {
	c := newFakeGW(t).client(t)
	for _, n := range []int{-1, 501} {
		if _, err := c.ListMessages(context.Background(), ListParams{Limit: n}); err == nil {
			t.Errorf("limit %d: expected error", n)
		}
	}
}

func TestCatchUpPagesUntilEmptyAndKeepsLastCursor(t *testing.T) {
	f := newFakeGW(t)
	pages := map[string]MessagePage{
		"":   {Cursor: "c1", Changes: []MessageChange{{ID: 1}, {ID: 2}}},
		"c1": {Cursor: "c2", Changes: []MessageChange{{ID: 3}}},
		"c2": {Cursor: "", Changes: nil},
	}
	f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, pages[r.URL.Query().Get("cursor")])
	})
	var seen []uint64
	cursor, err := f.client(t).CatchUp(context.Background(), ListParams{Folder: FolderInbox}, func(ch MessageChange) error {
		seen = append(seen, ch.ID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if cursor != "c2" {
		t.Errorf("cursor = %q, want c2 (last non-empty)", cursor)
	}
	if len(seen) != 3 {
		t.Errorf("seen = %v", seen)
	}
}

func TestCatchUpStopsOnHandlerError(t *testing.T) {
	f := newFakeGW(t)
	f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, MessagePage{Cursor: "next", Changes: []MessageChange{{ID: 1}, {ID: 2}}})
	})
	boom := context.Canceled
	cursor, err := f.client(t).CatchUp(context.Background(), ListParams{Cursor: "start"}, func(ch MessageChange) error {
		if ch.ID == 2 {
			return boom
		}
		return nil
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if cursor != "start" {
		t.Errorf("cursor = %q, want start (page not fully handled)", cursor)
	}
}

func TestCatchUpBoundsPages(t *testing.T) {
	f := newFakeGW(t)
	n := 0
	f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
		n++
		writeTestJSON(w, http.StatusOK, MessagePage{Cursor: strconv.Itoa(n), Changes: []MessageChange{{ID: uint64(n)}}})
	})
	_, err := f.client(t).CatchUp(context.Background(), ListParams{}, func(MessageChange) error { return nil })
	if err == nil {
		t.Fatal("expected page-limit error")
	}
	if n != maxCatchUpPages {
		t.Errorf("pages fetched = %d, want %d", n, maxCatchUpPages)
	}
}

func TestPreferencesAndMaxText(t *testing.T) {
	f := newFakeGW(t)
	override := 0
	f.mux.HandleFunc("GET /api/messages/preferences", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, http.StatusOK, MessagePreferences{MaxMessageTextOverride: override, RetryMaxAttempts: 4})
	})
	c := f.client(t)
	ctx := context.Background()

	p, err := c.MessagePreferences(ctx)
	if err != nil || p.RetryMaxAttempts != 4 {
		t.Fatalf("%+v %v", p, err)
	}
	if got := p.MaxText(); got != MaxMessageText {
		t.Errorf("MaxText = %d, want %d", got, MaxMessageText)
	}
	override = 150
	p, _ = c.MessagePreferences(ctx)
	if got := p.MaxText(); got != 150 {
		t.Errorf("MaxText = %d, want 150", got)
	}
	override = 999
	p, _ = c.MessagePreferences(ctx)
	if got := p.MaxText(); got != MaxMessageText {
		t.Errorf("out-of-range override: MaxText = %d, want default", got)
	}
}

func TestConversationPrefs(t *testing.T) {
	f := newFakeGW(t)
	stored := ConversationPrefs{WaitForAck: true}
	f.mux.HandleFunc("GET /api/messages/conversations/{kind}/{key}/prefs", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("kind") != "dm" || r.PathValue("key") != "HQ-1" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeTestJSON(w, http.StatusOK, stored)
	})
	f.mux.HandleFunc("PUT /api/messages/conversations/{kind}/{key}/prefs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&stored)
		writeTestJSON(w, http.StatusOK, stored)
	})
	c := f.client(t)
	ctx := context.Background()

	p, err := c.ConversationPrefs(ctx, ThreadKindDM, "hq-1")
	if err != nil || !p.WaitForAck {
		t.Fatalf("get: %+v %v", p, err)
	}
	p, err = c.SetConversationPrefs(ctx, ThreadKindDM, "hq-1", ConversationPrefs{WaitForAck: false})
	if err != nil || p.WaitForAck {
		t.Fatalf("put: %+v %v", p, err)
	}
	if _, err := c.SetConversationPrefs(ctx, ThreadKindDM, "hq-1", ConversationPrefs{SendPath: "carrier_pigeon"}); err == nil {
		t.Error("bad send_path: expected error")
	}
	if _, err := c.ConversationPrefs(ctx, "group", "HQ"); err == nil {
		t.Error("bad kind: expected error")
	}
	if _, err := c.ConversationPrefs(ctx, ThreadKindDM, "../etc"); err == nil {
		t.Error("bad key: expected error")
	}
}

func TestCatchUpDetectsStalledCursor(t *testing.T) {
	for _, next := range []string{"", "same"} {
		f := newFakeGW(t)
		f.mux.HandleFunc("GET /api/messages", func(w http.ResponseWriter, r *http.Request) {
			writeTestJSON(w, http.StatusOK, MessagePage{Cursor: next, Changes: []MessageChange{{ID: 1}}})
		})
		n := 0
		cursor, err := f.client(t).CatchUp(context.Background(), ListParams{Cursor: "same"}, func(MessageChange) error {
			n++
			return nil
		})
		if !errors.Is(err, ErrCursorStalled) {
			t.Errorf("next=%q: err = %v, want ErrCursorStalled", next, err)
		}
		if cursor != "same" || n != 1 {
			t.Errorf("next=%q: cursor = %q, handled = %d", next, cursor, n)
		}
	}
}

func TestAddresseeValidation(t *testing.T) {
	valid := []string{"HQ", "n0call", "N0CALL-9", "AID-3", "FINISH", "N0CALL-15"}
	invalid := []string{"", "-", "--", "-HQ", "N0 CALL", "TOOLONGTAC1", "HQ/1"}
	for _, a := range valid {
		if _, err := normalizeAddressee(a); err != nil {
			t.Errorf("%q: unexpected error %v", a, err)
		}
	}
	for _, a := range invalid {
		if _, err := normalizeAddressee(a); err == nil {
			t.Errorf("%q: expected error", a)
		}
	}
}
