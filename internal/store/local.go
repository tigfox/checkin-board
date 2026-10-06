package store

import (
	"checkin-board/internal/wire"
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"
)

// Checkpoint-side persistence: the local entry log and the RC1 outbox.

// maxEntriesPerBatch bounds how many queued rows CreateBatch loads; a
// 200-char frame holds ~25 entries, so this always covers a full batch.
const maxEntriesPerBatch = 64

// VoidOutcome says how VoidLocal cancelled an entry.
type VoidOutcome int

const (
	// VoidDeleted: the entry hadn't been batched yet, so it was removed
	// and nothing goes on air.
	VoidDeleted VoidOutcome = iota + 1
	// VoidQueued: the entry was already sent, so a void row was queued
	// to tell HQ.
	VoidQueued
)

// LocalEntryView is a local entry as the keypad shows it.
type LocalEntryView struct {
	LocalEntry
	Voided    bool   // a void has been logged for this entry
	VoidState string // delivery state of that void ("" if not voided)
}

// OutboxStats summarizes delivery for the keypad status strip.
type OutboxStats struct {
	Queued          int        // rows not yet in a batch
	Unconfirmed     int        // rows HQ hasn't ACKed (queued + sent)
	PendingBatches  int        // batches awaiting ACK
	RejectedBatches int        // batches HQ refused (REJ); need operator attention
	LastAckAt       *time.Time // most recent ACK from HQ
}

// ClientID is the correlation token a batch is sent with (graywolf
// echoes it), used to find a batch's row after a crash.
func ClientID(cp string, seq uint32) string { return fmt.Sprintf("%s-%d", cp, seq) }

// LogLocal records a bib at checkpoint cp. timeIn comes from the race
// clock; synced says whether that clock was trustworthy.
func (s *Store) LogLocal(ctx context.Context, cp string, bib Bib, timeIn time.Time, synced bool) (*LocalEntry, error) {
	if !wire.ValidCheckpointCode(cp) {
		return nil, fmt.Errorf("%w: checkpoint code %q", ErrInvalidInput, cp)
	}
	if !bib.Valid() {
		return nil, fmt.Errorf("%w: bib %d", ErrInvalidInput, bib)
	}
	e := &LocalEntry{
		CPCode:      cp,
		Bib:         bib,
		TimeIn:      normTime(timeIn),
		ClockSynced: synced,
		State:       EntryQueued,
		CreatedAt:   normTime(s.now()),
	}
	if err := s.db.WithContext(ctx).Create(e).Error; err != nil {
		return nil, err
	}
	return e, nil
}

// VoidLocal cancels entry id. An unsent entry is deleted; a sent one
// gets a void row queued for HQ. Void rows themselves can't be voided.
func (s *Store) VoidLocal(ctx context.Context, id uint) (VoidOutcome, error) {
	var outcome VoidOutcome
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e LocalEntry
		if err := tx.Where("id = ? AND void_of IS NULL", id).First(&e).Error; err != nil {
			return mapDBError(err)
		}
		var voids int64
		if err := tx.Model(&LocalEntry{}).Where("void_of = ?", id).Count(&voids).Error; err != nil {
			return err
		}
		if voids > 0 {
			return ErrAlreadyVoided
		}
		if e.State == EntryQueued {
			outcome = VoidDeleted
			return tx.Delete(&LocalEntry{}, id).Error
		}
		outcome = VoidQueued
		return tx.Create(&LocalEntry{
			CPCode:      e.CPCode,
			Bib:         e.Bib,
			TimeIn:      normTime(e.TimeIn),
			ClockSynced: e.ClockSynced,
			State:       EntryQueued,
			VoidOf:      &e.ID,
			CreatedAt:   normTime(s.now()),
		}).Error
	})
	if err != nil {
		return 0, err
	}
	return outcome, nil
}

// ListLocal returns the newest limit entries (void rows folded into
// their originals), newest first.
func (s *Store) ListLocal(ctx context.Context, limit int) ([]LocalEntryView, error) {
	var rows []LocalEntry
	if err := s.db.WithContext(ctx).Where("void_of IS NULL").
		Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]uint, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	var voids []LocalEntry
	if err := s.db.WithContext(ctx).Where("void_of IN ?", ids).Find(&voids).Error; err != nil {
		return nil, err
	}
	voidState := make(map[uint]string, len(voids))
	for _, v := range voids {
		voidState[*v.VoidOf] = v.State
	}
	out := make([]LocalEntryView, len(rows))
	for i, r := range rows {
		r.TimeIn, r.CreatedAt = normTime(r.TimeIn), normTime(r.CreatedAt)
		st, voided := voidState[r.ID]
		out[i] = LocalEntryView{LocalEntry: r, Voided: voided, VoidState: st}
	}
	return out, nil
}

// CreateBatch packs the oldest queued rows for cp into the next RC1
// report and marks them sent. It returns nil when nothing is queued.
// The batch is due for transmission immediately.
func (s *Store) CreateBatch(ctx context.Context, cp string, maxLen int, now time.Time) (*Batch, error) {
	var batch *Batch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rows []LocalEntry
		if err := tx.Where("cp_code = ? AND state = ?", cp, EntryQueued).
			Order("id ASC").Limit(maxEntriesPerBatch).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		last, err := lastSeq(tx, cp)
		if err != nil {
			return err
		}
		batchEntries := make([]Entry, len(rows))
		for i, r := range rows {
			batchEntries[i] = Entry{Bib: r.Bib, Time: wire.TimeOfDayOf(r.TimeIn), Void: r.VoidOf != nil}
		}
		text, n, err := wire.PackReport(cp, last+1, batchEntries, maxLen)
		if err != nil {
			return err
		}
		b := &Batch{
			CPCode:    cp,
			Seq:       last + 1,
			Text:      text,
			State:     BatchPending,
			ClientID:  ClientID(cp, last+1),
			NextTxAt:  normTime(now),
			CreatedAt: normTime(s.now()),
		}
		if err := tx.Create(b).Error; err != nil {
			return err
		}
		ids := make([]uint, n)
		for i := range ids {
			ids[i] = rows[i].ID
		}
		if err := tx.Model(&LocalEntry{}).Where("id IN ?", ids).
			Updates(map[string]any{"state": EntrySent, "batch_id": b.ID}).Error; err != nil {
			return err
		}
		batch = b
		return nil
	})
	return batch, err
}

// GetLocalEntry loads a logged entry (not a void row) by id.
func (s *Store) GetLocalEntry(ctx context.Context, id uint) (*LocalEntry, error) {
	var e LocalEntry
	if err := s.db.WithContext(ctx).Where("id = ? AND void_of IS NULL", id).First(&e).Error; err != nil {
		return nil, mapDBError(err)
	}
	e.TimeIn, e.CreatedAt = normTime(e.TimeIn), normTime(e.CreatedAt)
	return &e, nil
}

// QueuedCodes lists checkpoint codes with entries awaiting batching.
func (s *Store) QueuedCodes(ctx context.Context) ([]string, error) {
	var codes []string
	err := s.db.WithContext(ctx).Model(&LocalEntry{}).Where("state = ?", EntryQueued).
		Distinct().Order("cp_code").Pluck("cp_code", &codes).Error
	return codes, err
}

// ExportRow is one line of a checkpoint export: an entry or a void,
// with the seq of the batch that carries it (0 if not yet batched).
type ExportRow struct {
	CP          string
	Seq         uint32
	GWMsgID     string // graywolf msgid of the batch's row, for cross-checking
	Void        bool
	Bib         Bib
	TimeIn      time.Time
	ClockSynced bool
}

// ExportRows returns every local event in log order (id ascending),
// which within a batch is exactly the order CreateBatch packed it in.
func (s *Store) ExportRows(ctx context.Context) ([]ExportRow, error) {
	var rows []LocalEntry
	if err := s.db.WithContext(ctx).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	var batches []Batch
	if err := s.db.WithContext(ctx).Find(&batches).Error; err != nil {
		return nil, err
	}
	seqOf := make(map[uint]uint32, len(batches))
	msgOf := make(map[uint]string, len(batches))
	for _, b := range batches {
		seqOf[b.ID], msgOf[b.ID] = b.Seq, b.GWMsgID
	}
	out := make([]ExportRow, len(rows))
	for i, r := range rows {
		var seq uint32
		var msg string
		if r.BatchID != nil {
			seq, msg = seqOf[*r.BatchID], msgOf[*r.BatchID]
		}
		out[i] = ExportRow{CP: r.CPCode, Seq: seq, GWMsgID: msg, Void: r.VoidOf != nil, Bib: r.Bib,
			TimeIn: normTime(r.TimeIn), ClockSynced: r.ClockSynced}
	}
	return out, nil
}

// QueueSummary reports how many rows for cp await batching and when
// the oldest was logged (zero time when none).
func (s *Store) QueueSummary(ctx context.Context, cp string) (int, time.Time, error) {
	db := s.db.WithContext(ctx)
	var n int64
	if err := db.Model(&LocalEntry{}).Where("cp_code = ? AND state = ?", cp, EntryQueued).
		Count(&n).Error; err != nil {
		return 0, time.Time{}, err
	}
	if n == 0 {
		return 0, time.Time{}, nil
	}
	var oldest LocalEntry
	if err := db.Where("cp_code = ? AND state = ?", cp, EntryQueued).
		Order("id ASC").First(&oldest).Error; err != nil {
		return 0, time.Time{}, err
	}
	return int(n), normTime(oldest.CreatedAt), nil
}

// LastSeq returns the highest batch seq assigned for cp (0 if none).
func (s *Store) LastSeq(ctx context.Context, cp string) (uint32, error) {
	return lastSeq(s.db.WithContext(ctx), cp)
}

func lastSeq(db *gorm.DB, cp string) (uint32, error) {
	var last int64
	err := db.Model(&Batch{}).Where("cp_code = ?", cp).
		Select("COALESCE(MAX(seq), 0)").Scan(&last).Error
	return uint32(last), err
}

// DueBatches returns pending batches whose next transmission time has
// arrived, oldest first.
func (s *Store) DueBatches(ctx context.Context, now time.Time, limit int) ([]Batch, error) {
	var out []Batch
	if err := s.db.WithContext(ctx).
		Where("state = ? AND next_tx_at <= ?", BatchPending, normTime(now)).
		Order("id ASC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		normBatch(&out[i])
	}
	return out, nil
}

// ListPendingBatches returns every unACKed batch, oldest first.
func (s *Store) ListPendingBatches(ctx context.Context) ([]Batch, error) {
	var out []Batch
	if err := s.db.WithContext(ctx).Where("state = ?", BatchPending).
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		normBatch(&out[i])
	}
	return out, nil
}

// MarkTransmitted records a transmission of batch id at `at` and
// schedules the next retry for `next`.
//
// Only a pending batch is updated: one ACKed (or parked) since the
// caller listed it returns ErrNotFound, so it isn't resent.
func (s *Store) MarkTransmitted(ctx context.Context, id uint, at, next time.Time) error {
	res := s.db.WithContext(ctx).Model(&Batch{}).Where("id = ? AND state = ?", id, BatchPending).Updates(map[string]any{
		"attempts":   gorm.Expr("attempts + 1"),
		"last_tx_at": normTime(at),
		"next_tx_at": normTime(next),
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// BindMessage records the graywolf message row batch id was sent as
// (spec 3.1): later retransmits resend that row, and graywolf's ACK
// status for it confirms the batch. Rebinding replaces the row, for
// when graywolf lost the original (resend 404) and the batch went out
// as a new message. The row is also recorded for post-race cleanup.
func (s *Store) BindMessage(ctx context.Context, id uint, gwID uint64, msgID string) error {
	if gwID == 0 {
		return fmt.Errorf("%w: graywolf message id must be non-zero", ErrInvalidInput)
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&Batch{}).Where("id = ?", id).
			Updates(map[string]any{"gw_message_id": gwID, "gw_msg_id": msgID})
		if err := rowsOrNotFound(res); err != nil {
			return err
		}
		return recordGWRow(tx, gwID, GWRowBatch, s.now())
	})
}

// AckBatchByMessage marks the pending batch bound to graywolf row gwID
// acked and its entries confirmed. A stale or duplicate ACK, a row that
// isn't one of our batches, or an ACK for a row the batch has since been
// rebound away from, returns nil, nil: not an error (the current row's
// ACK still confirms it).
func (s *Store) AckBatchByMessage(ctx context.Context, gwID uint64, at time.Time) (*Batch, error) {
	var acked *Batch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		b, err := findPendingByMessage(tx, gwID)
		if err != nil || b == nil {
			return err
		}
		ackedAt := normTime(at)
		if err := tx.Model(&Batch{}).Where("id = ?", b.ID).
			Updates(map[string]any{"state": BatchAcked, "acked_at": ackedAt}).Error; err != nil {
			return err
		}
		if err := tx.Model(&LocalEntry{}).Where("batch_id = ?", b.ID).
			Update("state", EntryConfirmed).Error; err != nil {
			return err
		}
		b.State, b.AckedAt = BatchAcked, &ackedAt
		normBatch(b)
		acked = b
		return nil
	})
	return acked, err
}

// RejectBatchByMessage parks the pending batch bound to gwID after a
// REJ (or graywolf refusing it), so it stops occupying the in-flight
// window. A later gap request (RequeueSeqs) revives it. Returns nil, nil
// when nothing matches.
func (s *Store) RejectBatchByMessage(ctx context.Context, gwID uint64) (*Batch, error) {
	var rejected *Batch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		b, err := findPendingByMessage(tx, gwID)
		if err != nil || b == nil {
			return err
		}
		if err := tx.Model(&Batch{}).Where("id = ?", b.ID).Update("state", BatchRejected).Error; err != nil {
			return err
		}
		b.State = BatchRejected
		normBatch(b)
		rejected = b
		return nil
	})
	return rejected, err
}

// RejectBatch parks pending batch id directly, for a batch graywolf
// refused before it got a message row (HTTP 400). Returns nil, nil if
// the batch isn't pending.
func (s *Store) RejectBatch(ctx context.Context, id uint) (*Batch, error) {
	var rejected *Batch
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var b Batch
		err := tx.Where("id = ? AND state = ?", id, BatchPending).First(&b).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := tx.Model(&Batch{}).Where("id = ?", b.ID).Update("state", BatchRejected).Error; err != nil {
			return err
		}
		b.State = BatchRejected
		normBatch(&b)
		rejected = &b
		return nil
	})
	return rejected, err
}

func findPendingByMessage(tx *gorm.DB, gwID uint64) (*Batch, error) {
	var b Batch
	err := tx.Where("state = ? AND gw_message_id = ?", BatchPending, gwID).First(&b).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// UnboundAttempted returns pending batches whose send was attempted but
// never bound to a graywolf row: the app may have crashed between POST
// and BindMessage. Startup matches them against graywolf's sent folder
// before sending again (spec 4.1.7).
func (s *Store) UnboundAttempted(ctx context.Context) ([]Batch, error) {
	var out []Batch
	if err := s.db.WithContext(ctx).
		Where("state = ? AND gw_message_id IS NULL AND attempts > 0", BatchPending).
		Order("id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		normBatch(&out[i])
	}
	return out, nil
}

// RestoreSchedule puts a batch's attempts, last_tx_at, and next_tx_at
// back to the values in prev. The engine records an attempt before
// transmitting and calls this if the send itself fails.
//
// It applies only while the batch is still pending and still carries
// the attempt being undone (last_tx_at == sentAt): if an ACK, a gap
// request or fast retransmit changed it meanwhile, that newer state
// wins and ErrNotFound is returned.
func (s *Store) RestoreSchedule(ctx context.Context, prev Batch, sentAt time.Time) error {
	var lastTx any
	if prev.LastTxAt != nil {
		lastTx = normTime(*prev.LastTxAt)
	}
	return rowsOrNotFound(s.db.WithContext(ctx).Model(&Batch{}).
		Where("id = ? AND state = ? AND last_tx_at = ?", prev.ID, BatchPending, normTime(sentAt)).Updates(map[string]any{
		"attempts":   prev.Attempts,
		"last_tx_at": lastTx,
		"next_tx_at": normTime(prev.NextTxAt),
	}))
}

// requeueMinAge is how recently a batch must not have been sent for a
// gap request to requeue it. HQ never re-asks for a seq sooner than its
// gap grace (at least 30 s, 90 s by default) after the last request, and
// the checkpoint answered that request with a send. A batch sent in the
// last minute is either still in flight or its resend is already on the
// way, so a spoofed or duplicated gap request can't resend it in a loop.
const requeueMinAge = 60 * time.Second

// RequeueSeqs answers an HQ gap request: the named batches (even ones
// already ACKed or rejected; HQ says it lacks them) become due now with
// the retry ladder cut back to its 60 s rung (attempts capped at 1, as
// in ExpediteUnacked), unless sent within requeueMinAge. Unknown seqs
// are ignored. Returns how many batches were requeued.
func (s *Store) RequeueSeqs(ctx context.Context, cp string, seqs []uint32, now time.Time) (int, error) {
	if len(seqs) == 0 {
		return 0, nil
	}
	var n int
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []uint
		if err := tx.Model(&Batch{}).
			Where("cp_code = ? AND seq IN ? AND (last_tx_at IS NULL OR last_tx_at <= ?)",
				cp, seqs, normTime(now.Add(-requeueMinAge))).
			Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		// graywolf's resend doesn't clear a row's acked/rejected state,
		// so resending an acked or rejected batch's old row would read as
		// confirmed (or refused) at once without HQ getting it. Release
		// those bindings: the batch goes out as a new message, and HQ's
		// (cp, seq, text) dedup makes the new msgid harmless. The old row
		// stays in gw_rows for post-race cleanup.
		if err := tx.Model(&Batch{}).Where("id IN ? AND state IN ?", ids, []string{BatchAcked, BatchRejected}).
			Updates(map[string]any{"gw_message_id": nil, "gw_msg_id": ""}).Error; err != nil {
			return err
		}
		if err := tx.Model(&Batch{}).Where("id IN ?", ids).Updates(map[string]any{
			"state":      BatchPending,
			"acked_at":   nil,
			"attempts":   gorm.Expr("MIN(attempts, 1)"),
			"next_tx_at": normTime(now),
		}).Error; err != nil {
			return err
		}
		if err := tx.Model(&LocalEntry{}).Where("batch_id IN ?", ids).
			Update("state", EntrySent).Error; err != nil {
			return err
		}
		n = len(ids)
		return nil
	})
	return n, err
}

// ExpediteUnacked makes every pending batch last transmitted at or
// before sentBefore due at now, with its retry ladder cut back to the
// first rung (attempts capped at 1, so a failed resend waits 60 s, not 30). The
// engine calls it when HQ is evidently reachable: a batch out that long
// without an ACK was lost, and waiting out a long backoff would only
// delay the runners in it. Returns how many batches were expedited.
func (s *Store) ExpediteUnacked(ctx context.Context, sentBefore, now time.Time) (int, error) {
	res := s.db.WithContext(ctx).Model(&Batch{}).
		Where("state = ? AND last_tx_at IS NOT NULL AND last_tx_at <= ?", BatchPending, normTime(sentBefore)).
		Updates(map[string]any{"next_tx_at": normTime(now), "attempts": gorm.Expr("MIN(attempts, 1)")})
	return int(res.RowsAffected), res.Error
}

// ExpediteAll makes every unconfirmed batch due now for a checkpoint's
// final check-in at HQ (spec 4.7): pending and parked (rejected)
// batches alike, with the retry ladder cut back to its first rung.
// Rejected batches lose their graywolf binding, since graywolf keeps a
// rejected row rejected across resends (spec 3.1); they go out as new
// messages. Returns how many batches were made due.
func (s *Store) ExpediteAll(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Batch{}).Where("state = ?", BatchRejected).
			Updates(map[string]any{"gw_message_id": nil, "gw_msg_id": "", "state": BatchPending}).Error; err != nil {
			return err
		}
		res := tx.Model(&Batch{}).Where("state = ?", BatchPending).
			Updates(map[string]any{"next_tx_at": normTime(now), "attempts": gorm.Expr("MIN(attempts, 1)")})
		n = int(res.RowsAffected)
		return res.Error
	})
	return n, err
}

// OutboxStats summarizes delivery state.
func (s *Store) OutboxStats(ctx context.Context) (OutboxStats, error) {
	var st OutboxStats
	db := s.db.WithContext(ctx)
	var queued, unconfirmed, pending int64
	if err := db.Model(&LocalEntry{}).Where("state = ?", EntryQueued).Count(&queued).Error; err != nil {
		return st, err
	}
	if err := db.Model(&LocalEntry{}).Where("state <> ?", EntryConfirmed).Count(&unconfirmed).Error; err != nil {
		return st, err
	}
	if err := db.Model(&Batch{}).Where("state = ?", BatchPending).Count(&pending).Error; err != nil {
		return st, err
	}
	var rejected int64
	if err := db.Model(&Batch{}).Where("state = ?", BatchRejected).Count(&rejected).Error; err != nil {
		return st, err
	}
	st.Queued, st.Unconfirmed, st.PendingBatches = int(queued), int(unconfirmed), int(pending)
	st.RejectedBatches = int(rejected)

	// Not MAX(acked_at): aggregates lose the DATETIME column type and
	// come back as strings. Order + First keeps the typed scan.
	var last Batch
	err := db.Where("acked_at IS NOT NULL").Order("acked_at DESC").First(&last).Error
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
	case err != nil:
		return st, err
	default:
		st.LastAckAt = normTimePtr(last.AckedAt)
	}
	return st, nil
}

func normBatch(b *Batch) {
	b.NextTxAt = normTime(b.NextTxAt)
	b.CreatedAt = normTime(b.CreatedAt)
	b.LastTxAt = normTimePtr(b.LastTxAt)
	b.AckedAt = normTimePtr(b.AckedAt)
}

// UnconfirmedRows returns the graywolf row ids bound to batches HQ has
// not confirmed (pending or parked). Post-race cleanup must not delete
// them: deleting a row cancels the resend that would deliver it.
func (s *Store) UnconfirmedRows(ctx context.Context) (map[uint64]bool, error) {
	var ids []uint64
	if err := s.db.WithContext(ctx).Model(&Batch{}).
		Where("state <> ? AND gw_message_id IS NOT NULL", BatchAcked).
		Pluck("gw_message_id", &ids).Error; err != nil {
		return nil, err
	}
	out := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out, nil
}
