package web

import (
	"net/http"
	"testing"
	"time"

	"checkin-board/internal/store"
)

func TestLinkCheckRequestAndList(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceSetup))
	resp := e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{"count": 3})
	c := decode[linkCheckView](t, resp)
	if c.ID == 0 || c.State != store.LinkCheckRequested || c.PeerCall != "N0CALL-10" || c.Count != 3 {
		t.Fatalf("created = %+v", c)
	}
	expect(t, e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{}), http.StatusConflict) // busy
	list := decode[linkCheckList](t, e.do("GET", "/api/admin/linkcheck", e.admin, nil))
	if len(list.Checks) != 1 || list.MaxCount == 0 || list.DefaultCount == 0 {
		t.Fatalf("list = %+v", list)
	}
	expect(t, e.do("POST", "/api/admin/linkcheck/999/cancel", e.admin, nil), http.StatusNotFound)
	expect(t, e.do("POST", "/api/admin/linkcheck/x/cancel", e.admin, nil), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/admin/linkcheck/1/cancel", e.admin, nil), http.StatusOK)
	got, _ := e.st.GetLinkCheck(ctx, c.ID)
	if got.State != store.LinkCheckCancelled {
		t.Fatalf("after cancel = %+v", got)
	}
	// Two minutes between runs.
	resp = e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{})
	expect(t, resp, http.StatusCreated) // a cancelled run doesn't count
	got, _ = e.st.GetLinkCheck(ctx, 2)
	fin := got.RequestedAt
	got.State, got.FinishedAt = store.LinkCheckDone, &fin
	_ = e.st.UpdateLinkCheck(ctx, got)
	resp = e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{})
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("too soon: %d %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestLinkCheckRules(t *testing.T) {
	e := newEnv(t, checkpointSettings(store.RaceActive))
	resp := e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("active without confirm = %d", resp.StatusCode)
	}
	if b := decode[errorBody](t, resp); b.Code != "confirm_needed" {
		t.Fatalf("code = %q", b.Code)
	}
	expect(t, e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{"confirm": true, "count": 99}), http.StatusBadRequest)
	expect(t, e.do("POST", "/api/admin/linkcheck", e.admin, map[string]any{"confirm": true}), http.StatusCreated)

	done := newEnv(t, checkpointSettings(store.RaceComplete))
	expect(t, done.do("POST", "/api/admin/linkcheck", done.admin, map[string]any{"confirm": true}), http.StatusConflict)
}

func TestLinkReadinessAndHealthColumn(t *testing.T) {
	e := newEnv(t, hqSettings(store.RaceSetup))
	if err := e.st.CreateCheckpoint(ctx, &store.Checkpoint{Code: "AS5", Name: "Ridge", CourseOrder: 1, ExpectedCall: "N0CALL-1"}); err != nil {
		t.Fatal(err)
	}
	r := decode[map[string][]string](t, e.do("GET", "/api/admin/linkcheck/readiness", e.admin, nil))
	if len(r["warnings"]) != 1 {
		t.Fatalf("readiness = %+v", r)
	}
	lvl := -24
	if _, err := e.st.RecordProbeHeard(ctx, store.ProbeHeard{PeerCall: "N0CALL-1", ProberCode: "AS5", Run: 3, Total: 2, Idx: 1, At: time.Now(), Level: &lvl}); err != nil {
		t.Fatal(err)
	}
	st := decode[map[string]any](t, e.do("GET", "/api/admin/status", e.admin, nil))
	links, _ := st["links"].(map[string]any)
	as5, _ := links["AS5"].(map[string]any)
	if as5["heard"] != float64(1) || as5["total"] != float64(2) || as5["side"] != "checkpoint" {
		t.Fatalf("status links = %+v", st["links"])
	}
}
