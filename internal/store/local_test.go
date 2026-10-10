package store

import (
	"checkin-board/internal/wire"
	"errors"
	"strconv"
	"testing"
	"time"
)

func mustLog(t *testing.T, s *Store, cp string, bib Bib, when time.Time) *LocalEntry {
	t.Helper()
	e, err := s.LogLocal(ctx, cp, bib, when, true)
	if err != nil {
		t.Fatalf("LogLocal(%s, %d): %v", cp, bib, err)
	}
	return e
}

func TestLogLocal(t *testing.T) {
	s := newTestStore(t)
	e, err := s.LogLocal(ctx, "AS5", 101, at(5*time.Second+700*time.Millisecond), false)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID == 0 || e.State != EntryQueued || e.ClockSynced || e.BatchID != nil || e.VoidOf != nil {
		t.Fatalf("unexpected entry: %+v", e)
	}
	// Stored as UTC, truncated to the second (the wire's resolution).
	if !e.TimeIn.Equal(at(5*time.Second)) || e.TimeIn.Location() != time.UTC {
		t.Fatalf("TimeIn = %v, want %v UTC", e.TimeIn, at(5*time.Second))
	}
}

func TestLogLocalValidates(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.LogLocal(ctx, "bad", 1, t0, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad cp: err = %v", err)
	}
	if _, err := s.LogLocal(ctx, "AS5", 0, t0, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("bad bib: err = %v", err)
	}
}

func TestCreateBatchPacksQueuedEntriesInOrder(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "AS5", 101, at(5*time.Second))
	mustLog(t, s, "AS5", 104, at(22*time.Second))
	mustLog(t, s, "OTHER", 9, at(30*time.Second)) // different code: not in this batch
	mustLog(t, s, "AS5", 57, at(62*time.Second))

	b, err := s.CreateBatch(ctx, "AS5", wire.DefaultMaxTextLen, at(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if b == nil || b.Seq != 1 || b.State != BatchPending || b.Attempts != 0 {
		t.Fatalf("batch = %+v", b)
	}
	if want := "RC1 R AS5 1 @1300 101/05 104/22 @1301 57/02"; b.Text != want {
		t.Fatalf("Text = %q, want %q", b.Text, want)
	}
	if !b.NextTxAt.Equal(at(time.Minute)) {
		t.Fatalf("NextTxAt = %v, want due immediately", b.NextTxAt)
	}

	views, err := s.ListLocal(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range views {
		wantState := EntrySent
		if v.CPCode == "OTHER" {
			wantState = EntryQueued
		}
		if v.State != wantState {
			t.Errorf("bib %d state = %q, want %q", v.Bib, v.State, wantState)
		}
	}

	// Nothing left to batch for AS5.
	if b2, err := s.CreateBatch(ctx, "AS5", wire.DefaultMaxTextLen, at(time.Minute)); err != nil || b2 != nil {
		t.Fatalf("second CreateBatch = %+v, %v; want nil, nil", b2, err)
	}
}

func TestCreateBatchSplitsAndIncrementsSeq(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 20; i++ {
		mustLog(t, s, "3", Bib(1000+i), at(time.Duration(i)*time.Second))
	}
	var total int
	for seq := uint32(1); ; seq++ {
		b, err := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
		if err != nil {
			t.Fatal(err)
		}
		if b == nil {
			break
		}
		if b.Seq != seq {
			t.Fatalf("seq = %d, want %d", b.Seq, seq)
		}
		if len(b.Text) > wire.DefaultMaxTextLen {
			t.Fatalf("batch %d too long: %q", seq, b.Text)
		}
		msg, err := wire.Decode(b.Text)
		if err != nil {
			t.Fatal(err)
		}
		total += len(msg.(*Report).Entries)
	}
	if total != 20 {
		t.Fatalf("entries across batches = %d, want 20", total)
	}
	if last, _ := s.LastSeq(ctx, "3"); last < 3 {
		t.Fatalf("LastSeq = %d, want >= 3", last)
	}
}

func TestSeqIsPerCheckpointCode(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "START", 1, t0)
	mustLog(t, s, "FIN", 1, t0)
	a, _ := s.CreateBatch(ctx, "START", wire.DefaultMaxTextLen, t0)
	b, _ := s.CreateBatch(ctx, "FIN", wire.DefaultMaxTextLen, t0)
	if a.Seq != 1 || b.Seq != 1 {
		t.Fatalf("seqs = %d, %d; want 1, 1", a.Seq, b.Seq)
	}
	if last, _ := s.LastSeq(ctx, "NONE"); last != 0 {
		t.Fatalf("LastSeq(unknown) = %d, want 0", last)
	}
}

func TestVoidQueuedEntryDeletesIt(t *testing.T) {
	s := newTestStore(t)
	e := mustLog(t, s, "3", 101, t0)
	out, err := s.VoidLocal(ctx, e.ID)
	if err != nil || out != VoidDeleted {
		t.Fatalf("VoidLocal = %v, %v; want VoidDeleted", out, err)
	}
	if b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0); b != nil {
		t.Fatalf("deleted entry was batched: %q", b.Text)
	}
}

func TestVoidSentEntryQueuesVoid(t *testing.T) {
	s := newTestStore(t)
	e := mustLog(t, s, "3", 101, at(5*time.Second))
	if _, err := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0); err != nil {
		t.Fatal(err)
	}
	out, err := s.VoidLocal(ctx, e.ID)
	if err != nil || out != VoidQueued {
		t.Fatalf("VoidLocal = %v, %v; want VoidQueued", out, err)
	}
	b, err := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	if err != nil || b == nil {
		t.Fatalf("void not batched: %v", err)
	}
	if want := "RC1 R 3 2 @1300 -101/05"; b.Text != want {
		t.Fatalf("Text = %q, want %q", b.Text, want)
	}

	views, _ := s.ListLocal(ctx, 10)
	if len(views) != 1 || !views[0].Voided || views[0].ID != e.ID {
		t.Fatalf("ListLocal = %+v; want only the original, marked voided", views)
	}

	if _, err := s.VoidLocal(ctx, e.ID); !errors.Is(err, ErrAlreadyVoided) {
		t.Fatalf("double void err = %v, want ErrAlreadyVoided", err)
	}
}

func TestVoidErrors(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.VoidLocal(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: err = %v, want ErrNotFound", err)
	}
	e := mustLog(t, s, "3", 101, t0)
	_, _ = s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	_, _ = s.VoidLocal(ctx, e.ID)
	var voidRow LocalEntry
	if err := s.db.Where("void_of = ?", e.ID).First(&voidRow).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.VoidLocal(ctx, voidRow.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("voiding a void row: err = %v, want ErrNotFound", err)
	}
}

func TestListLocalNewestFirstWithLimit(t *testing.T) {
	s := newTestStore(t)
	for i := 1; i <= 5; i++ {
		mustLog(t, s, "3", Bib(i), at(time.Duration(i)*time.Second))
	}
	views, err := s.ListLocal(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 || views[0].Bib != 5 || views[2].Bib != 3 {
		t.Fatalf("ListLocal = %+v", views)
	}
}

func TestDueBatchesAndMarkTransmitted(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)

	due, err := s.DueBatches(ctx, t0, 10)
	if err != nil || len(due) != 1 || due[0].ID != b.ID {
		t.Fatalf("DueBatches = %+v, %v", due, err)
	}
	if err := s.MarkTransmitted(ctx, b.ID, t0, at(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.DueBatches(ctx, at(29*time.Second), 10); len(due) != 0 {
		t.Fatalf("batch due before backoff expired: %+v", due)
	}
	due, _ = s.DueBatches(ctx, at(30*time.Second), 10)
	if len(due) != 1 || due[0].Attempts != 1 || due[0].LastTxAt == nil || !due[0].LastTxAt.Equal(t0) {
		t.Fatalf("after MarkTransmitted: %+v", due)
	}
	if err := s.MarkTransmitted(ctx, 999, t0, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing batch err = %v", err)
	}
}

func TestAckBatchConfirmsEntries(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	mustLog(t, s, "3", 2, t0)
	b, _ := sendBatch(t, s, "3", t0)

	if got, err := s.AckBatchByMessage(ctx, 999999, at(time.Second)); err != nil || got != nil {
		t.Fatalf("ack for a row that isn't ours = %+v, %v; want nil", got, err)
	}
	got, err := s.AckBatchByMessage(ctx, gwIDOf(b), at(time.Second))
	if err != nil || got == nil || got.ID != b.ID || got.State != BatchAcked || got.AckedAt == nil {
		t.Fatalf("AckBatchByMessage = %+v, %v", got, err)
	}
	views, _ := s.ListLocal(ctx, 10)
	for _, v := range views {
		if v.State != EntryConfirmed {
			t.Fatalf("bib %d state = %q, want confirmed", v.Bib, v.State)
		}
	}
	if due, _ := s.DueBatches(ctx, at(time.Hour), 10); len(due) != 0 {
		t.Fatalf("acked batch still due: %+v", due)
	}
	// A duplicate ACK is a no-op.
	if again, err := s.AckBatchByMessage(ctx, gwIDOf(b), at(2*time.Second)); err != nil || again != nil {
		t.Fatalf("duplicate ack = %+v, %v; want nil", again, err)
	}
}

func TestRequeueSeqsResendsEvenAckedBatches(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 3; i++ {
		mustLog(t, s, "3", Bib(i+1), t0)
		if _, err := sendBatch(t, s, "3", t0); err != nil {
			t.Fatal(err)
		}
	}
	for seq := uint32(1); seq <= 3; seq++ {
		ackSeq(t, s, "3", seq, t0)
	}
	// HQ lost batch 2 (e.g. its DB was restored) and asks again; seq 9 is unknown.
	n, err := s.RequeueSeqs(ctx, "3", []uint32{2, 9}, at(time.Minute))
	if err != nil || n != 1 {
		t.Fatalf("RequeueSeqs = %d, %v; want 1", n, err)
	}
	due, _ := s.DueBatches(ctx, at(time.Minute), 10)
	if len(due) != 1 || due[0].Seq != 2 || due[0].State != BatchPending || due[0].AckedAt != nil {
		t.Fatalf("due after requeue = %+v", due)
	}
	views, _ := s.ListLocal(ctx, 10)
	for _, v := range views {
		want := EntryConfirmed
		if v.Bib == 2 {
			want = EntrySent
		}
		if v.State != want {
			t.Errorf("bib %d state = %q, want %q", v.Bib, v.State, want)
		}
	}
}

func TestOutboxStats(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	mustLog(t, s, "3", 2, t0)
	b, _ := sendBatch(t, s, "3", t0)
	mustLog(t, s, "3", 3, t0) // still queued

	st, err := s.OutboxStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Queued != 1 || st.Unconfirmed != 3 || st.PendingBatches != 1 || st.LastAckAt != nil {
		t.Fatalf("stats = %+v", st)
	}
	ackSeq(t, s, "3", b.Seq, at(time.Second))
	st, _ = s.OutboxStats(ctx)
	if st.Queued != 1 || st.Unconfirmed != 1 || st.PendingBatches != 0 ||
		st.LastAckAt == nil || !st.LastAckAt.Equal(at(time.Second)) {
		t.Fatalf("stats after ack = %+v", st)
	}
}

func TestListPendingBatches(t *testing.T) {
	s := newTestStore(t)
	for i := 1; i <= 2; i++ {
		mustLog(t, s, "3", Bib(i), t0)
		if _, err := sendBatch(t, s, "3", at(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	ackSeq(t, s, "3", 1, t0)
	// Unlike DueBatches, pending batches are listed regardless of next_tx_at.
	got, err := s.ListPendingBatches(ctx)
	if err != nil || len(got) != 1 || got[0].Seq != 2 {
		t.Fatalf("ListPendingBatches = %+v, %v", got, err)
	}
}

func TestRequeueSeqsNoop(t *testing.T) {
	s := newTestStore(t)
	if n, err := s.RequeueSeqs(ctx, "3", nil, t0); n != 0 || err != nil {
		t.Fatalf("empty = %d, %v", n, err)
	}
	if n, err := s.RequeueSeqs(ctx, "3", []uint32{1, 2}, t0); n != 0 || err != nil {
		t.Fatalf("unknown seqs = %d, %v", n, err)
	}
}

func TestQueueSummary(t *testing.T) {
	s := newTestStore(t)
	ft := &fakeTime{t: t0}
	s.now = ft.Now
	if n, oldest, err := s.QueueSummary(ctx, "3"); err != nil || n != 0 || !oldest.IsZero() {
		t.Fatalf("empty = %d, %v, %v", n, oldest, err)
	}
	mustLog(t, s, "3", 1, t0)
	ft.Advance(10 * time.Second)
	mustLog(t, s, "3", 2, t0)
	mustLog(t, s, "OTHER", 3, t0)
	n, oldest, err := s.QueueSummary(ctx, "3")
	if err != nil || n != 2 || !oldest.Equal(t0) {
		t.Fatalf("QueueSummary = %d, %v, %v; want 2 queued since %v", n, oldest, err, t0)
	}
}

func TestRejectBatchLeavesWindow(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := sendBatch(t, s, "3", t0)
	got, err := s.RejectBatchByMessage(ctx, gwIDOf(b))
	if err != nil || got == nil || got.State != BatchRejected {
		t.Fatalf("RejectBatchByMessage = %+v, %v", got, err)
	}
	if pending, _ := s.ListPendingBatches(ctx); len(pending) != 0 {
		t.Fatalf("rejected batch still pending: %+v", pending)
	}
	if st, _ := s.OutboxStats(ctx); st.RejectedBatches != 1 || st.PendingBatches != 0 {
		t.Fatalf("stats = %+v", st)
	}
	// A rejected batch can't be ACKed, but a later gap request revives it.
	if again, _ := s.AckBatchByMessage(ctx, gwIDOf(b), t0); again != nil {
		t.Fatal("rejected batch was acked")
	}
	if n, _ := s.RequeueSeqs(ctx, "3", []uint32{b.Seq}, at(requeueMinAge)); n != 1 {
		t.Fatal("gap request did not revive the rejected batch")
	}
	if missing, _ := s.RejectBatchByMessage(ctx, 999999); missing != nil {
		t.Fatal("rejecting an unknown row returned a batch")
	}
}

func TestRejectBatchByIDBeforeBinding(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	got, err := s.RejectBatch(ctx, b.ID)
	if err != nil || got == nil || got.State != BatchRejected {
		t.Fatalf("RejectBatch = %+v, %v", got, err)
	}
	if again, err := s.RejectBatch(ctx, b.ID); err != nil || again != nil {
		t.Fatalf("second RejectBatch = %+v, %v; want nil", again, err)
	}
}

func TestRequeueResetsAttempts(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := sendBatch(t, s, "3", t0)
	for i := 0; i < 4; i++ {
		_ = s.MarkTransmitted(ctx, b.ID, t0, t0)
	}
	ackSeq(t, s, "3", b.Seq, t0)
	_, _ = s.RequeueSeqs(ctx, "3", []uint32{b.Seq}, at(requeueMinAge))
	due, _ := s.DueBatches(ctx, at(requeueMinAge), 1)
	if len(due) != 1 || due[0].Attempts != 1 {
		t.Fatalf("requeued batch = %+v, want attempts cut back to 1", due)
	}
}

func TestRestoreSchedule(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	if err := s.MarkTransmitted(ctx, b.ID, at(time.Second), at(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RestoreSchedule(ctx, *b, at(time.Second)); err != nil {
		t.Fatal(err)
	}
	due, _ := s.DueBatches(ctx, t0, 1)
	if len(due) != 1 || due[0].Attempts != 0 || due[0].LastTxAt != nil || !due[0].NextTxAt.Equal(t0) {
		t.Fatalf("after restore = %+v, want the original schedule", due)
	}
}

// sendBatch creates the next batch for cp and records one transmission,
// as the engine would. ACKs and REJs only match transmitted batches.
func sendBatch(t *testing.T, s *Store, cp string, now time.Time) (*Batch, error) {
	t.Helper()
	b, err := s.CreateBatch(ctx, cp, wire.DefaultMaxTextLen, now)
	if err != nil || b == nil {
		return b, err
	}
	if err := s.MarkTransmitted(ctx, b.ID, now, now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	gwID := gwIDOf(b)
	if err := s.BindMessage(ctx, b.ID, gwID, "1"); err != nil {
		t.Fatal(err)
	}
	b.Attempts, b.GWMessageID = 1, &gwID
	return b, nil
}

// gwIDOf is the fake graywolf row id tests bind batch b to.
func gwIDOf(b *Batch) uint64 { return 1000 + uint64(b.ID) }

// ackSeq ACKs cp's batch seq via its bound graywolf row.
func ackSeq(t *testing.T, s *Store, cp string, seq uint32, now time.Time) {
	t.Helper()
	var b Batch
	if err := s.db.Where("cp_code = ? AND seq = ?", cp, seq).First(&b).Error; err != nil {
		t.Fatalf("batch %s/%d: %v", cp, seq, err)
	}
	if b.GWMessageID == nil {
		t.Fatalf("batch %s/%d was never sent", cp, seq)
	}
	if _, err := s.AckBatchByMessage(ctx, *b.GWMessageID, now); err != nil {
		t.Fatal(err)
	}
}

// An ACK only answers a batch bound to that graywolf row: an unbound
// batch (never sent) can't be confirmed by any row's ACK.
func TestAckIgnoresUnboundBatches(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	if _, err := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{1, 2, 1000} {
		if got, err := s.AckBatchByMessage(ctx, id, t0); err != nil || got != nil {
			t.Fatalf("acked an unbound batch via row %d: %+v, %v", id, got, err)
		}
	}
}

func TestBindMessage(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	if b.ClientID != "3-1" {
		t.Errorf("ClientID = %q, want 3-1", b.ClientID)
	}
	if err := s.BindMessage(ctx, b.ID, 0, "1"); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero id err = %v", err)
	}
	if err := s.BindMessage(ctx, 999, 5, "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing batch err = %v", err)
	}
	if err := s.BindMessage(ctx, b.ID, 50, "17"); err != nil {
		t.Fatal(err)
	}
	// graywolf refused a resend of row 50; the batch went out again as row 51.
	if err := s.BindMessage(ctx, b.ID, 51, "18"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AckBatchByMessage(ctx, 51, t0)
	if err != nil || got == nil || got.GWMsgID != "18" {
		t.Fatalf("ack via new row = %+v, %v", got, err)
	}
	rows, _ := s.ListGWRows(ctx)
	if len(rows) != 2 || rows[0].Kind != GWRowBatch {
		t.Fatalf("gw rows = %+v, want both rows recorded for cleanup", rows)
	}
	// Two batches can't share a graywolf row.
	mustLog(t, s, "3", 2, t0)
	b2, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	if err := s.BindMessage(ctx, b2.ID, 51, "18"); !errors.Is(err, ErrConflict) {
		t.Fatalf("shared row err = %v, want ErrConflict", err)
	}
}

// Each retransmit is a new graywolf row (graywolf won't resend while
// retries are off), so HQ may ACK any copy: an ACK for an earlier copy
// confirms the batch, but only the current copy's REJ parks it.
func TestAckByEarlierCopy(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	for _, id := range []uint64{50, 51} {
		if err := s.BindMessage(ctx, b.ID, id, strconv.FormatUint(id, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.RejectBatchByMessage(ctx, 50); got != nil {
		t.Fatal("REJ for an earlier copy parked the batch")
	}
	keep, err := s.UnconfirmedRows(ctx)
	if err != nil || !keep[50] || !keep[51] {
		t.Fatalf("UnconfirmedRows = %v, %v; want both copies kept from cleanup", keep, err)
	}
	got, err := s.AckBatchByMessage(ctx, 50, t0)
	if err != nil || got == nil || got.ID != b.ID || got.State != BatchAcked {
		t.Fatalf("ACK via earlier copy = %+v, %v; want the batch confirmed", got, err)
	}
	if again, _ := s.AckBatchByMessage(ctx, 51, t0); again != nil {
		t.Fatal("second copy's ACK confirmed an already acked batch")
	}
	if keep, _ := s.UnconfirmedRows(ctx); keep[50] || keep[51] {
		t.Fatalf("UnconfirmedRows = %v; confirmed batch's copies should be free to clean up", keep)
	}
}

// HQ ACKed an earlier copy, but the newest was refused and parked the
// batch: HQ has it, so the ACK still confirms it.
func TestAckByEarlierCopyConfirmsParkedBatch(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	_ = s.BindMessage(ctx, b.ID, 70, "70")
	_ = s.BindMessage(ctx, b.ID, 71, "71")
	if got, _ := s.RejectBatchByMessage(ctx, 71); got == nil {
		t.Fatal("REJ of the newest copy didn't park the batch")
	}
	got, err := s.AckBatchByMessage(ctx, 70, t0)
	if err != nil || got == nil || got.State != BatchAcked {
		t.Fatalf("ACK via earlier copy of a parked batch = %+v, %v; want confirmed", got, err)
	}
}

func TestReleaseBindingAndCopies(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	for _, id := range []uint64{80, 81, 82, 83} {
		_ = s.BindMessage(ctx, b.ID, id, strconv.FormatUint(id, 10))
	}
	ids, err := s.EarlierCopies(ctx, b.ID, 2)
	if err != nil || len(ids) != 2 || ids[0] != 82 || ids[1] != 81 {
		t.Fatalf("EarlierCopies = %v, %v; want [82 81] (newest first, current excluded)", ids, err)
	}
	if err := s.ReleaseBinding(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.ListPendingBatches(ctx)
	if pending[0].GWMessageID != nil || pending[0].GWMsgID != "" {
		t.Fatalf("batch = %+v, want unbound", pending[0])
	}
	if got, _ := s.AckBatchByMessage(ctx, 83, t0); got == nil {
		t.Fatal("released copy's ACK didn't confirm the batch: it is still a linked copy")
	}
	if err := s.ReleaseBinding(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing batch err = %v", err)
	}
}

// A gap request says HQ never got the batch: its earlier copies are
// forgotten, so a replayed ACK for one of them can't confirm it.
func TestRequeueForgetsEarlierCopies(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := sendBatch(t, s, "3", t0)
	if err := s.BindMessage(ctx, b.ID, 61, "61"); err != nil { // a retransmit
		t.Fatal(err)
	}
	if n, err := s.RequeueSeqs(ctx, "3", []uint32{b.Seq}, at(time.Hour)); err != nil || n != 1 {
		t.Fatalf("RequeueSeqs = %d, %v", n, err)
	}
	if err := s.BindMessage(ctx, b.ID, 62, "62"); err != nil { // sent again after the gap request
		t.Fatal(err)
	}
	if got, _ := s.AckBatchByMessage(ctx, 61, at(time.Hour)); got != nil {
		t.Fatal("ACK for a copy sent before the gap request confirmed the batch")
	}
	if got, _ := s.AckBatchByMessage(ctx, 62, at(time.Hour)); got == nil {
		t.Fatal("ACK for the copy sent after the gap request didn't confirm the batch")
	}
}

func TestUnboundAttempted(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	never, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0) // not attempted yet
	mustLog(t, s, "3", 2, t0)
	crashed, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	_ = s.MarkTransmitted(ctx, crashed.ID, t0, at(30*time.Second)) // attempted, crash before bind
	mustLog(t, s, "3", 3, t0)
	bound, _ := sendBatch(t, s, "3", t0)

	got, err := s.UnboundAttempted(ctx)
	if err != nil || len(got) != 1 || got[0].ID != crashed.ID {
		t.Fatalf("UnboundAttempted = %+v, %v; want only batch %d (not %d or %d)", got, err, crashed.ID, never.ID, bound.ID)
	}
}

func TestExpediteUnacked(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	old, _ := sendBatch(t, s, "3", t0) // sent at t0, backed off
	for i := 0; i < 3; i++ {
		_ = s.MarkTransmitted(ctx, old.ID, t0, at(time.Hour))
	}
	mustLog(t, s, "3", 2, t0)
	fresh, _ := sendBatch(t, s, "3", at(50*time.Second)) // just sent; ACK may still be coming
	mustLog(t, s, "3", 3, t0)
	unsent, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, at(time.Hour))

	n, err := s.ExpediteUnacked(ctx, at(40*time.Second), at(60*time.Second))
	if err != nil || n != 1 {
		t.Fatalf("ExpediteUnacked = %d, %v; want only the stale batch", n, err)
	}
	due, _ := s.DueBatches(ctx, at(60*time.Second), 10)
	if len(due) != 1 || due[0].ID != old.ID || due[0].Attempts != 1 {
		t.Fatalf("due = %+v, want the stale batch with attempts cut to 1", due)
	}
	_, _ = fresh, unsent
}

// A spoofed or duplicated gap request must not resend a batch that just
// went out.
func TestRequeueSkipsRecentlySent(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := sendBatch(t, s, "3", t0)
	ackSeq(t, s, "3", b.Seq, t0)
	if n, _ := s.RequeueSeqs(ctx, "3", []uint32{b.Seq}, at(requeueMinAge-time.Second)); n != 0 {
		t.Fatalf("requeued a batch sent %v ago", requeueMinAge-time.Second)
	}
	if n, _ := s.RequeueSeqs(ctx, "3", []uint32{b.Seq}, at(requeueMinAge)); n != 1 {
		t.Fatal("did not requeue once requeueMinAge had passed")
	}
}

// A gap request for an acked batch must release its graywolf row:
// graywolf's resend keeps the row's acked state, so resending it would
// look confirmed without HQ receiving anything.
func TestRequeueReleasesAckedAndRejectedBindings(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	acked, _ := sendBatch(t, s, "3", t0)
	ackSeq(t, s, "3", acked.Seq, t0)
	mustLog(t, s, "3", 2, t0)
	rejected, _ := sendBatch(t, s, "3", t0)
	_, _ = s.RejectBatchByMessage(ctx, gwIDOf(rejected))
	mustLog(t, s, "3", 3, t0)
	pending, _ := sendBatch(t, s, "3", t0)

	if n, err := s.RequeueSeqs(ctx, "3", []uint32{1, 2, 3}, at(requeueMinAge)); err != nil || n != 3 {
		t.Fatalf("RequeueSeqs = %d, %v", n, err)
	}
	due, _ := s.DueBatches(ctx, at(requeueMinAge), 10)
	if len(due) != 3 {
		t.Fatalf("due = %+v", due)
	}
	for _, b := range due {
		bound := b.GWMessageID != nil
		if wantBound := b.ID == pending.ID; bound != wantBound {
			t.Errorf("batch %d (seq %d) bound=%v, want %v", b.ID, b.Seq, bound, wantBound)
		}
	}
	// Released rows stay recorded for post-race cleanup.
	if rows, _ := s.ListGWRows(ctx); len(rows) != 3 {
		t.Errorf("gw rows = %d, want 3", len(rows))
	}
}

func TestRestoreScheduleYieldsToNewerState(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := s.CreateBatch(ctx, "3", wire.DefaultMaxTextLen, t0)
	_ = s.MarkTransmitted(ctx, b.ID, at(time.Second), at(time.Minute))
	// Fast retransmit rescheduled it meanwhile (same last_tx_at kept, but
	// a later attempt at a different time is a different attempt).
	_ = s.MarkTransmitted(ctx, b.ID, at(2*time.Second), at(time.Minute))
	if err := s.RestoreSchedule(ctx, *b, at(time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stale restore err = %v, want ErrNotFound", err)
	}
	if due, _ := s.DueBatches(ctx, at(time.Hour), 1); len(due) != 1 || due[0].Attempts != 2 {
		t.Fatalf("batch = %+v, want the newer attempt kept", due)
	}
}

func TestMarkTransmittedSkipsNonPending(t *testing.T) {
	s := newTestStore(t)
	mustLog(t, s, "3", 1, t0)
	b, _ := sendBatch(t, s, "3", t0)
	ackSeq(t, s, "3", b.Seq, t0)
	if err := s.MarkTransmitted(ctx, b.ID, at(time.Second), at(time.Minute)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an acked batch", err)
	}
}
