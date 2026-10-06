package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
)

func cliDB(t *testing.T, state string) (string, *store.Store) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "checkin-board.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c := store.DefaultSettings()
	c.Role, c.CheckpointCode, c.HQCall, c.RaceState = store.RoleCheckpoint, "AS5", "N0CALL-10", state
	if _, err := st.SaveSettings(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	return db, st
}

// fakeService finishes the requested run with verdict, as the running
// service would.
func fakeService(t *testing.T, st *store.Store, verdict string) {
	t.Helper()
	go func() {
		ctx := context.Background()
		for range 200 {
			c, err := st.NextRequestedLinkCheck(ctx)
			if err == nil {
				now := time.Now()
				rtt := 4200
				c.State, c.StartedAt, c.FinishedAt, c.Verdict = store.LinkCheckDone, &now, &now, verdict
				c.Uplink, c.RoundTrip, c.ReplyReceived, c.MedianRTTms = 5, 5, true, &rtt
				_ = st.UpdateLinkCheck(ctx, c)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
}

var fast = linkcheck.Timing{Poll: 10 * time.Millisecond, StartWait: 300 * time.Millisecond, MaxWait: 5 * time.Second}

func TestLinkCheckCLIExitCodes(t *testing.T) {
	for verdict, want := range map[string]int{linkcheck.Pass: 0, linkcheck.Marginal: 1, linkcheck.Fail: 2} {
		db, st := cliDB(t, store.RaceSetup)
		fakeService(t, st, verdict)
		var out bytes.Buffer
		if code := runLinkCheckCLI(context.Background(), db, nil, &out, fast); code != want {
			t.Errorf("%s: exit %d, want %d (%s)", verdict, code, want, out.String())
		}
		if !strings.Contains(out.String(), verdict) || !strings.Contains(out.String(), "N0CALL-10") {
			t.Errorf("%s: output %q", verdict, out.String())
		}
	}
}

func TestLinkCheckCLIJSON(t *testing.T) {
	db, st := cliDB(t, store.RaceSetup)
	fakeService(t, st, linkcheck.Pass)
	var out bytes.Buffer
	if code := runLinkCheckCLI(context.Background(), db, []string{"--json", "--count", "5"}, &out, fast); code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var v map[string]any
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v["verdict"] != "PASS" || v["median_rtt_ms"] != float64(4200) {
		t.Fatalf("json = %v, %v (%s)", v, err, out.String())
	}
}

func TestLinkCheckCLINoService(t *testing.T) {
	db, st := cliDB(t, store.RaceSetup)
	var out bytes.Buffer
	if code := runLinkCheckCLI(context.Background(), db, nil, &out, fast); code != exitFail {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "service") {
		t.Fatalf("output %q", out.String())
	}
	c, _ := st.GetLinkCheck(context.Background(), 1)
	if c.State != store.LinkCheckCancelled {
		t.Fatalf("request left behind: %+v", c)
	}
}

func TestLinkCheckCLIRefusals(t *testing.T) {
	db, _ := cliDB(t, store.RaceActive)
	var out bytes.Buffer
	if code := runLinkCheckCLI(context.Background(), db, nil, &out, fast); code != exitFail || !strings.Contains(out.String(), "--yes") {
		t.Fatalf("active race without --yes: exit %d, %q", code, out.String())
	}
	out.Reset()
	if code := runLinkCheckCLI(context.Background(), db, []string{"--bogus"}, &out, fast); code != exitFail {
		t.Fatalf("bad flag: exit %d", code)
	}
}

func TestLinkCheckCLIBriefAndDBFlag(t *testing.T) {
	db, st := cliDB(t, store.RaceSetup)
	fakeService(t, st, linkcheck.Marginal)
	var out bytes.Buffer
	code := runLinkCheckCLI(context.Background(), "/nonexistent/ignored.db", []string{"--db", db, "--brief"}, &out, fast)
	line := strings.TrimSpace(out.String())
	if code != exitMarginal || strings.Count(line, "\n") != 0 || len(line) > 50 || !strings.HasPrefix(line, "MARGINAL N0CALL-10") {
		t.Fatalf("exit %d, brief %q (%d chars)", code, line, len(line))
	}
}
