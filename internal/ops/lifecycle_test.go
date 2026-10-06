package ops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
)

func TestLifecycleTransitions(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceSetup)
	if _, err := e.svc.Complete(ctx); !errors.Is(err, ErrWrongState) {
		t.Fatalf("complete from setup err = %v", err)
	}
	cfg, err := e.svc.Start(ctx)
	if err != nil || cfg.RaceState != store.RaceActive || cfg.RaceStartedAt == nil || !cfg.RaceStartedAt.Equal(t0) {
		t.Fatalf("Start = %+v, %v", cfg, err)
	}
	if _, err := e.svc.Start(ctx); !errors.Is(err, ErrWrongState) {
		t.Fatalf("second Start err = %v", err)
	}
	e.logBib("AS5", 1)
	if _, err := e.svc.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	sec, err := e.svc.Secure(ctx)
	if err != nil || sec.Settings.RaceState != store.RaceSecured || sec.Unconfirmed != 1 {
		t.Fatalf("Secure = %+v, %v", sec, err)
	}
	if cfg, err := e.svc.CheckIn(ctx); err != nil || cfg.RaceState != store.RaceCheckingIn {
		t.Fatalf("CheckIn = %+v, %v", cfg, err)
	}
	// Packing up again mid check-in is allowed.
	if _, err := e.svc.Secure(ctx); err != nil {
		t.Fatalf("re-secure: %v", err)
	}
}

func TestLifecycleRoleRules(t *testing.T) {
	hqe := newEnv(t, hqSettings(), store.RaceActive)
	if _, err := hqe.svc.Secure(ctx); !errors.Is(err, ErrWrongRole) {
		t.Errorf("HQ secure err = %v", err)
	}
	if _, err := hqe.svc.CheckIn(ctx); !errors.Is(err, ErrWrongRole) {
		t.Errorf("HQ check-in err = %v", err)
	}
	unset := newEnv(t, store.DefaultSettings(), store.RaceSetup)
	if _, err := unset.svc.Start(ctx); !errors.Is(err, ErrWrongRole) {
		t.Errorf("start without a role err = %v", err)
	}
}

func TestCheckInFromCompleteWithoutSecuring(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceComplete)
	if _, err := e.svc.CheckIn(ctx); err != nil {
		t.Fatalf("check-in from complete: %v", err)
	}
}

// sendAll runs the checkpoint engine so logged bibs go out as batches.
func (e *env) sendAll() {
	e.t.Helper()
	cfg, _ := e.st.GetSettings(ctx)
	e.ft.Advance(30 * time.Second)
	if err := e.cp.Tick(ctx, cfg); err != nil {
		e.t.Fatal(err)
	}
}

func TestCleanupGraywolfKeepsWhatHQStillNeeds(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	e.logBib("AS5", 1)
	e.sendAll() // batch 1 and a heartbeat
	batches := e.gw.TransmissionsWithPrefix("RC1 R ")
	e.gw.Ack(batches[0].ID)
	m, _ := e.gw.GetMessage(ctx, batches[0].ID)
	_ = e.cp.HandleOutbound(ctx, m)
	e.logBib("AS5", 2)
	e.sendAll()                                                        // batch 2, never ACKed
	operator := e.gw.Inbound("N0CALL-10", "73, see you at the finish") // operator chat: never ours

	if _, err := e.svc.CleanupGraywolf(ctx); !errors.Is(err, ErrWrongState) {
		t.Fatalf("cleanup during the race err = %v", err)
	}
	if _, err := e.svc.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.CleanupGraywolf(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Deleted < 2 || res.Kept != 1 || res.Failed != 0 {
		t.Fatalf("cleanup = %+v, want the ACKed batch and heartbeat deleted, the unconfirmed batch kept", res)
	}
	if _, err := e.gw.GetMessage(ctx, operator.ID); err != nil {
		t.Fatal("operator's own message deleted")
	}
	pending := e.gw.TransmissionsWithPrefix("RC1 R ")
	if _, err := e.gw.GetMessage(ctx, pending[len(pending)-1].ID); err != nil {
		t.Fatal("unconfirmed batch's row deleted; its resend would be cancelled")
	}
	// Repeatable: nothing more to delete.
	again, _ := e.svc.CleanupGraywolf(ctx)
	if again.Deleted != 0 || again.Kept != 1 {
		t.Fatalf("second cleanup = %+v", again)
	}
}

func TestCleanupCountsRowsAlreadyGone(t *testing.T) {
	e := newEnv(t, hqSettings(), store.RaceComplete)
	gone := uint64(4242)
	if _, err := e.st.RecordGWRow(ctx, gone, store.GWRowGap); err != nil {
		t.Fatal(err)
	}
	res, err := e.svc.CleanupGraywolf(ctx)
	if err != nil || res.Gone != 1 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	if rows, _ := e.st.ListGWRows(ctx); len(rows) != 0 {
		t.Fatalf("gone row not marked deleted: %+v", rows)
	}
}

func TestResetRequiresConfirmationAndGuardsUnsentData(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	e.logBib("AS5", 1)
	if _, err := e.svc.Reset(ctx, ResetRequest{Confirm: "wrong"}); !errors.Is(err, ErrConfirmMismatch) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.svc.Reset(ctx, ResetRequest{Confirm: "ridge 50k"}); !errors.Is(err, ErrUnsentData) {
		t.Fatalf("err = %v, want the unsent-data guard", err)
	}
	if _, err := e.svc.Reset(ctx, ResetRequest{Confirm: "Ridge 50K", AcknowledgeUnsent: true}); err != nil {
		t.Fatalf("acknowledged reset: %v", err)
	}
	unnamed := newEnv(t, hqSettings(), store.RaceActive)
	_, _ = unnamed.st.SaveSettings(ctx, func() store.Settings { c := hqSettings(); c.RaceName = ""; return c }())
	if _, err := unnamed.svc.Reset(ctx, ResetRequest{Confirm: "reset"}); err != nil {
		t.Fatalf("reset of an unnamed race with RESET: %v", err)
	}
}

func TestResetBacksUpAndClears(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	_, _ = e.gw.SetConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10", graywolf.ConversationPrefs{SendPath: "rf_only", WaitForAck: true})
	if err := e.svc.cfg.Peers.Ensure(ctx, "N0CALL-10"); err != nil {
		t.Fatal(err)
	}
	e.logBib("AS5", 1)
	e.sendAll()
	e.gw.Ack(e.gw.TransmissionsWithPrefix("RC1 R ")[0].ID)
	m, _ := e.gw.GetMessage(ctx, e.gw.TransmissionsWithPrefix("RC1 R ")[0].ID)
	_ = e.cp.HandleOutbound(ctx, m)
	_, _ = e.svc.Complete(ctx)

	res, err := e.svc.Reset(ctx, ResetRequest{Confirm: "Ridge 50K"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v", res.Warnings)
	}
	for _, f := range []string{"checkin-board.db", "export.csv", "race-journal.csv"} {
		if fi, err := os.Stat(filepath.Join(res.BackupDir, f)); err != nil || fi.Size() == 0 {
			t.Errorf("backup %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(e.dir, "race-journal.csv")); !os.IsNotExist(err) {
		t.Error("journal not moved out for the next race")
	}
	if got, _ := e.svc.RecentEntries(ctx, 10); len(got) != 0 {
		t.Errorf("entries survived: %+v", got)
	}
	if e.state() != store.RaceSetup {
		t.Errorf("state = %s", e.state())
	}
	if p, _ := e.gw.ConversationPrefs(ctx, graywolf.ThreadKindDM, "N0CALL-10"); !p.WaitForAck || p.SendPath != "rf_only" {
		t.Errorf("graywolf prefs not restored: %+v", p)
	}
	if e.clock.Status().Source != "unsynced" {
		t.Error("race clock sync survived the reset")
	}
	if since, _ := e.st.InboxSince(ctx); !since.Equal(t0.Add(30 * time.Second)) {
		t.Errorf("inbox starting point = %v, want moved to the reset time", since)
	}
	// The next race's journal starts fresh.
	_, _ = e.svc.Start(ctx)
	e.logBib("AS5", 9)
	if recs := readJournal(t, e); len(recs) != 1 || recs[0].Bib != 9 {
		t.Fatalf("new journal = %+v", recs)
	}
}

func TestResetOfCheckedInNodeNeedsNoAcknowledgement(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceCheckedIn)
	if _, err := e.st.LogLocal(ctx, "AS5", 1, t0, true); err != nil { // e.g. confirmed later by export
		t.Fatal(err)
	}
	if _, err := e.svc.Reset(ctx, ResetRequest{Confirm: "Ridge 50K"}); err != nil {
		t.Fatalf("reset after check-in: %v", err)
	}
}

func TestHQResetWritesResults(t *testing.T) {
	e := seedHQ(t)
	res, err := e.svc.Reset(ctx, ResetRequest{Confirm: "Ridge 50K", ClearReference: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.BackupDir, "results.csv")); err != nil {
		t.Fatal(err)
	}
	if cps, _ := e.st.ListCheckpoints(ctx); len(cps) != 0 {
		t.Fatal("checkpoint list kept with ClearReference")
	}
}

func TestSlug(t *testing.T) {
	cases := map[string]string{"Ridge 50K": "Ridge-50K", "": "race", "  ": "race", "a/b\\c": "a-b-c"}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
	if got := slug("x" + string(make([]byte, 60))); len(got) > 40 {
		t.Errorf("slug too long: %d", len(got))
	}
}

func TestCleanupBlockedDuringCheckIn(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceCheckingIn)
	if _, err := e.svc.CleanupGraywolf(ctx); !errors.Is(err, ErrWrongState) {
		t.Fatalf("err = %v", err)
	}
}

func TestTransitionLosesToConcurrentChange(t *testing.T) {
	e := newEnv(t, checkpointSettings(), store.RaceActive)
	// The settings say active, but by the time the CAS runs it's complete.
	if ok, _ := e.st.SetRaceState(ctx, []string{store.RaceActive}, store.RaceComplete, nil); !ok {
		t.Fatal("setup")
	}
	if _, err := e.svc.Complete(ctx); !errors.Is(err, ErrWrongState) {
		t.Fatalf("err = %v", err)
	}
}
