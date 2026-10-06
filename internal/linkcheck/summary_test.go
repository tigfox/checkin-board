package linkcheck

import (
	"strings"
	"testing"
	"time"

	"checkin-board/internal/store"
)

func ptr[T any](v T) *T { return &v }

func TestLatestPerCheckpointEitherSide(t *testing.T) {
	cps := []store.Checkpoint{
		{Code: "AS1", Name: "Creek", ExpectedCall: "K1CP"},
		{Code: "AS2", Name: "Ridge", ExpectedCall: "K2CP"},
		{Code: "AS3", Name: "Summit", ExpectedCall: "K3CP"},
	}
	checks := []store.LinkCheck{ // HQ probed AS1 twice
		{PeerCall: "K1CP", State: store.LinkCheckDone, Verdict: Marginal, Count: 5, Uplink: 3, FinishedAt: ptr(t0), LocalLevel: ptr(-25)},
		{PeerCall: "K1CP", State: store.LinkCheckDone, Verdict: Pass, Count: 5, Uplink: 5, FinishedAt: ptr(t0.Add(time.Hour)), LocalLevel: ptr(-22)},
		{PeerCall: "K2CP", State: store.LinkCheckCancelled, FinishedAt: ptr(t0)},
	}
	responses := []store.LinkResponse{ // AS2 probed HQ; AS1 too, but earlier than HQ's run
		{ProberCode: "AS2", PeerCall: "K2CP", Total: 5, Heard: "1,2,3,4,5", LastHeardAt: t0, ReplyAckedAt: ptr(t0), Level: ptr(-30)},
		{ProberCode: "AS1", PeerCall: "K1CP", Total: 5, Heard: "1", LastHeardAt: t0.Add(30 * time.Minute)},
	}
	got := Latest(cps, checks, responses)
	if s := got["AS1"]; s.Verdict != Pass || s.Side != SideHQ || s.Heard != 5 || *s.Level != -22 {
		t.Errorf("AS1 = %+v", s)
	}
	if s := got["AS2"]; s.Verdict != Pass || s.Side != SideCheckpoint || s.Heard != 5 || *s.Level != -30 {
		t.Errorf("AS2 = %+v", s)
	}
	if _, ok := got["AS3"]; ok {
		t.Error("AS3 never checked but has a result")
	}
}

func TestReadinessWarnings(t *testing.T) {
	now := t0.Add(3 * time.Hour)
	cps := []store.Checkpoint{{Code: "AS1", Name: "Creek", ExpectedCall: "K1CP"}, {Code: "AS2", Name: "Ridge", ExpectedCall: "K2CP"}}
	hq := store.Settings{Role: store.RoleHQ}
	checks := []store.LinkCheck{
		{PeerCall: "K1CP", State: store.LinkCheckDone, Verdict: Pass, Count: 5, Uplink: 5, FinishedAt: ptr(now.Add(-time.Hour))},
		{PeerCall: "K2CP", State: store.LinkCheckDone, Verdict: Pass, Count: 5, Uplink: 5, FinishedAt: ptr(now.Add(-3 * time.Hour))},
	}
	w := Readiness(hq, cps, checks, nil, now)
	if len(w) != 1 || !strings.Contains(w[0], "AS2") {
		t.Fatalf("HQ warnings = %v", w)
	}
	cp := store.Settings{Role: store.RoleCheckpoint, HQCall: "N0HQ"}
	if w := Readiness(cp, nil, nil, nil, now); len(w) != 1 {
		t.Fatalf("checkpoint with no check: %v", w)
	}
	ok := []store.LinkCheck{{PeerCall: "N0HQ", State: store.LinkCheckDone, Verdict: Pass, FinishedAt: ptr(now.Add(-time.Minute))}}
	if w := Readiness(cp, nil, ok, nil, now); len(w) != 0 {
		t.Fatalf("checkpoint with a recent PASS: %v", w)
	}
	// HQ probing this checkpoint counts too (the checkpoint answered it).
	resp := []store.LinkResponse{{PeerCall: "N0HQ", ProberCode: HQCode, Total: 5, Heard: "1,2,3,4,5", LastHeardAt: now.Add(-time.Minute), ReplyAckedAt: ptr(now)}}
	if w := Readiness(cp, nil, nil, resp, now); len(w) != 0 {
		t.Fatalf("checkpoint answered a passing HQ check: %v", w)
	}
}
