package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"checkin-board/internal/wire"
)

func TestBackupAndResetRaceData(t *testing.T) {
	s := newTestStore(t)
	cfg := DefaultSettings()
	cfg.Role, cfg.CheckpointCode, cfg.HQCall, cfg.RaceState, cfg.RaceName = RoleCheckpoint, "AS5", "N0CALL-10", RaceActive, "Ridge"
	started := t0
	cfg.RaceStartedAt = &started
	if _, err := s.SaveSettings(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	mustLog(t, s, "AS5", 1, t0)
	b, _ := sendBatch(t, s, "AS5", t0)
	_ = s.CreateCheckpoint(ctx, &Checkpoint{Code: "AS5", Name: "Aid 5"})
	_, _ = s.UpsertRunners(ctx, []Runner{{Bib: 1}})
	msg, _ := wire.Decode("RC1 R AS9 1 @1300 7/00")
	_, _ = s.IngestReport(ctx, msg.(*wire.Report), "N0CALL-9", 50, t0)

	path := filepath.Join(t.TempDir(), "snap.db")
	if err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, path); err == nil {
		t.Fatal("backup overwrote an existing file")
	}
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("backup file: %v %v", fi, err)
	}

	if err := s.ResetRaceData(ctx, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if views, _ := s.ListLocal(ctx, 10); len(views) != 0 {
		t.Errorf("local entries survived: %+v", views)
	}
	if p, _ := s.ListPendingBatches(ctx); len(p) != 0 {
		t.Errorf("batches survived: %+v", p)
	}
	if e, _ := s.EffectiveEntries(ctx, EntryFilter{}); len(e) != 0 {
		t.Errorf("HQ entries survived: %+v", e)
	}
	if st, _ := s.ListStatuses(ctx); len(st) != 0 {
		t.Errorf("statuses survived: %+v", st)
	}
	got, _ := s.GetSettings(ctx)
	if got.RaceState != RaceSetup || got.RaceStartedAt != nil || got.RaceName != "Ridge" || got.CheckpointCode != "AS5" {
		t.Errorf("settings after reset = %+v", got)
	}
	if cps, _ := s.ListCheckpoints(ctx); len(cps) != 1 {
		t.Errorf("checkpoint list cleared without ClearReference")
	}
	for _, id := range []uint64{gwIDOf(b), 50} {
		if known, _ := s.KnownGWRow(ctx, id); !known {
			t.Errorf("graywolf row %d forgotten; cleanup could no longer delete it", id)
		}
	}
	// The seq counter restarts: the next batch is #1 again.
	mustLog(t, s, "AS5", 2, t0)
	if nb, _ := s.CreateBatch(ctx, "AS5", 67, t0); nb == nil || nb.Seq != 1 {
		t.Errorf("seq after reset = %+v", nb)
	}

	if err := s.ResetRaceData(ctx, ResetOptions{ClearReference: true}); err != nil {
		t.Fatal(err)
	}
	if cps, _ := s.ListCheckpoints(ctx); len(cps) != 0 {
		t.Error("checkpoint list kept with ClearReference")
	}
	if rs, _ := s.ListRunners(ctx); len(rs) != 0 {
		t.Error("roster kept with ClearReference")
	}
}

func TestExpediteAllRevivesRejectedAsNew(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	pending, _ := sendBatch(t, s, "3", t0)
	_ = s.MarkTransmitted(ctx, pending.ID, t0, at(time.Hour)) // backed off for an hour
	mustLog(t, s, "3", 2, t0)
	rejected, _ := sendBatch(t, s, "3", t0)
	_, _ = s.RejectBatchByMessage(ctx, gwIDOf(rejected))

	n, err := s.ExpediteAll(ctx, at(time.Minute))
	if err != nil || n != 2 {
		t.Fatalf("ExpediteAll = %d, %v", n, err)
	}
	due, _ := s.DueBatches(ctx, at(time.Minute), 10)
	if len(due) != 2 {
		t.Fatalf("due = %+v", due)
	}
	for _, b := range due {
		if b.Attempts > 1 {
			t.Errorf("batch %d attempts = %d, want the ladder cut back", b.Seq, b.Attempts)
		}
		if b.ID == rejected.ID && b.GWMessageID != nil {
			t.Error("rejected batch kept its graywolf row; it would read as rejected again")
		}
		if b.ID == pending.ID && b.GWMessageID == nil {
			t.Error("pending batch lost its graywolf row")
		}
	}
}

func TestExportRowsCarryGraywolfMsgID(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	_, _ = sendBatch(t, s, "3", t0) // bound with msgid "1"
	rows, _ := s.ExportRows(ctx)
	if len(rows) != 1 || rows[0].GWMsgID != "1" {
		t.Fatalf("rows = %+v", rows)
	}
}

func TestListHQLocal(t *testing.T) {
	s := newTestStore(t)
	a, _ := s.LogHQLocal(ctx, "FIN", 7, t0)
	_, _ = s.LogHQLocal(ctx, "FIN", 7, t0) // double tap, same second
	_, _ = s.LogHQLocal(ctx, "FIN", 8, at(time.Second))
	if err := s.VoidHQLocal(ctx, a.ID, at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListHQLocal(ctx, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("ListHQLocal = %+v, %v", got, err)
	}
	if got[0].Bib != 8 || got[0].Voided || !got[1].Voided || got[2].Voided {
		t.Fatalf("newest-first with the newest double tap voided: %+v", got)
	}
	if got, _ := s.ListHQLocal(ctx, 1); len(got) != 1 {
		t.Fatalf("limit ignored: %+v", got)
	}
}

func TestSettingsRaceStatesByRole(t *testing.T) {
	cp := DefaultSettings()
	cp.Role, cp.CheckpointCode, cp.HQCall = RoleCheckpoint, "AS5", "N0CALL-10"
	for _, st := range []string{RaceSecured, RaceCheckingIn, RaceCheckedIn} {
		cp.RaceState = st
		if err := cp.Validate(); err != nil {
			t.Errorf("checkpoint %s: %v", st, err)
		}
	}
	hq := DefaultSettings()
	hq.Role, hq.HQLocalCodes, hq.RaceState = RoleHQ, "FIN", RaceSecured
	if err := hq.Validate(); err == nil {
		t.Error("HQ accepted a checkpoint-only state")
	}
}

func TestUnconfirmedRows(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	acked, _ := sendBatch(t, s, "3", t0)
	ackSeq(t, s, "3", acked.Seq, t0)
	mustLog(t, s, "3", 2, t0)
	pending, _ := sendBatch(t, s, "3", t0)
	got, err := s.UnconfirmedRows(ctx)
	if err != nil || len(got) != 1 || !got[gwIDOf(pending)] {
		t.Fatalf("UnconfirmedRows = %v, %v", got, err)
	}
}

func TestBrandingPersistence(t *testing.T) {
	s := newTestStore(t)
	if b, err := s.Branding(ctx); err != nil || b.HeaderText != "" {
		t.Fatalf("fresh = %+v, %v", b, err)
	}
	if _, err := s.SaveBranding(ctx, BrandingRow{HeaderText: "Ridge 50K", ColorPrimary: "#112233"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Branding(ctx); b.HeaderText != "Ridge 50K" || b.ColorPrimary != "#112233" {
		t.Fatalf("branding = %+v", b)
	}
	if _, err := s.Logo(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no logo err = %v", err)
	}
	if err := s.SaveLogo(ctx, LogoRow{Image: []byte{1, 2}, ContentType: "image/png", Width: 1, Height: 1, SHA256: "ab"}); err != nil {
		t.Fatal(err)
	}
	if l, err := s.Logo(ctx); err != nil || len(l.Image) != 2 {
		t.Fatalf("logo = %+v, %v", l, err)
	}
	if err := s.DeleteLogo(ctx); err != nil {
		t.Fatal(err)
	}
	_ = s.SaveLogo(ctx, LogoRow{Image: []byte{1}, ContentType: "image/png", SHA256: "x"})
	if err := s.ClearBranding(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Branding(ctx); b.HeaderText != "" {
		t.Fatal("branding survived ClearBranding")
	}
	if _, err := s.Logo(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("logo survived ClearBranding")
	}
}

func TestResetClearBranding(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.SaveBranding(ctx, BrandingRow{HeaderText: "X"})
	if err := s.ResetRaceData(ctx, ResetOptions{}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Branding(ctx); b.HeaderText != "X" {
		t.Fatal("branding cleared without ClearBranding")
	}
	if err := s.ResetRaceData(ctx, ResetOptions{ClearBranding: true}); err != nil {
		t.Fatal(err)
	}
	if b, _ := s.Branding(ctx); b.HeaderText != "" {
		t.Fatal("branding kept with ClearBranding")
	}
}
