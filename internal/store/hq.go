package store

import (
	"checkin-board/internal/wire"
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// HQ-side persistence: batch dedup, the entry event log, and
// per-checkpoint status.
//
// Entries are an append-only log of entry and void events. The
// effective set is computed by netting events per (cp, bib, time_in),
// which makes ingest order-independent (a void may arrive before its
// original via a gap resend) and keeps a double-tapped bib alive when
// the volunteer voids only one copy.

// maxHeartbeatSeqJump bounds how far beyond the highest seq HQ has seen
// a heartbeat's lastseq (or a report's seq) may claim before HQ ignores
// it for gap detection. 500 batches is several hours of mass-start
// traffic, far more than any outage builds up; anything larger is a
// corrupt or spoofed packet that would otherwise make HQ request
// batches that never existed.
const maxHeartbeatSeqJump = 500

// IngestResult reports what IngestReport did.
type IngestResult struct {
	Duplicate bool // exact batch already ingested; nothing stored
	SeqReused bool // known seq with new text (checkpoint DB likely reset); stored anyway
	Entries   int  // entry/void events stored
}

// EntryFilter narrows EffectiveEntries. Zero value means everything.
type EntryFilter struct {
	Bib    *Bib
	CPCode string
}

// EffectiveEntry is one runner passage after voids are netted out.
type EffectiveEntry struct {
	CPCode     string
	Bib        Bib
	TimeIn     time.Time
	Count      int       // >1 when the same bib/time was logged more than once
	SourceCall string    // "" for HQ's own keypad
	ReceivedAt time.Time // first arrival
}

// IngestReport stores a decoded report from source, received at
// receivedAt (HQ race clock). Time-of-day values are resolved to the
// instant nearest receivedAt. gwID is the graywolf inbox row it came
// from (0 = none); it is recorded in the same transaction, so the inbox
// reader can skip the row once the batch is stored.
func (s *Store) IngestReport(ctx context.Context, rep *Report, source string, gwID uint64, receivedAt time.Time) (IngestResult, error) {
	recv := normTime(receivedAt)
	resolve := func(i int) time.Time { return rep.Entries[i].Time.Resolve(recv) }
	return ingest(s.db.WithContext(ctx), rep, source, gwID, resolve, receivedAt, true)
}

// IngestImported stores a batch rebuilt from a checkpoint export. The
// export carries each row's exact time, so times[i] is used for
// rep.Entries[i] instead of re-resolving a time of day (a batch can
// hold a void of an entry logged more than 12 h earlier). The batch
// still dedups against radio copies exactly like IngestReport, on its
// canonical text. It does not count as hearing the checkpoint, so gap
// requests keep going to the station's real callsign.
func (s *Store) IngestImported(ctx context.Context, rep *Report, times []time.Time, receivedAt time.Time) (IngestResult, error) {
	return ingestImported(s.db.WithContext(ctx), ImportedBatch{Report: rep, Times: times}, receivedAt)
}

// ImportedBatch is one batch rebuilt from a checkpoint export, with the
// exact time of each entry (parallel to Report.Entries).
type ImportedBatch struct {
	Report *Report
	Times  []time.Time
}

// IngestImportedBatches stores a whole checkpoint export in one
// transaction: all of it or none of it, and one fsync on an SD card
// instead of one per batch. Results are per batch, in order.
func (s *Store) IngestImportedBatches(ctx context.Context, batches []ImportedBatch, receivedAt time.Time) ([]IngestResult, error) {
	out := make([]IngestResult, 0, len(batches))
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, b := range batches {
			res, err := ingestImported(tx, b, receivedAt)
			if err != nil {
				return fmt.Errorf("%s batch %d: %w", b.Report.CP, b.Report.Seq, err)
			}
			out = append(out, res)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func ingestImported(db *gorm.DB, b ImportedBatch, receivedAt time.Time) (IngestResult, error) {
	if len(b.Times) != len(b.Report.Entries) {
		return IngestResult{}, fmt.Errorf("%w: %d times for %d entries", ErrInvalidInput, len(b.Times), len(b.Report.Entries))
	}
	resolve := func(i int) time.Time { return normTime(b.Times[i]) }
	return ingest(db, b.Report, SourceImport, 0, resolve, receivedAt, false)
}

// Source labels for entries that did not arrive over the air.
const (
	SourceImport  = "IMPORT"  // from a checkpoint export file
	SourceJournal = "JOURNAL" // reconciled from a bib journal
)

// ingest stores one batch in a transaction on db (a savepoint when db
// is itself a transaction).
func ingest(db *gorm.DB, rep *Report, source string, gwID uint64, resolve func(i int) time.Time, receivedAt time.Time, isHeard bool) (IngestResult, error) {
	var res IngestResult
	// Decode is text-canonical, so re-packing reproduces the exact
	// on-air text: the batch-level dedup key.
	text, n, err := wire.PackReport(rep.CP, rep.Seq, rep.Entries, 1<<16)
	if err != nil || n != len(rep.Entries) {
		return res, fmt.Errorf("%w: report does not re-encode (packed %d of %d entries): %w", ErrInvalidInput, n, len(rep.Entries), err)
	}
	recv := normTime(receivedAt)
	markHeard := func(st *CheckpointStatus) {
		if isHeard {
			heard(st, source, recv)
		}
	}
	var gwRef *uint64
	if gwID != 0 {
		gwRef = &gwID
	}
	err = db.Transaction(func(tx *gorm.DB) error {
		if gwID != 0 {
			if err := recordGWRow(tx, gwID, GWRowInbound, recv); err != nil {
				return err
			}
		}
		rb := &ReceivedBatch{CPCode: rep.CP, Seq: rep.Seq, Text: text, SourceCall: source, GWMessageID: gwRef, ReceivedAt: recv}
		ins := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(rb)
		if ins.Error != nil {
			return ins.Error
		}
		if ins.RowsAffected == 0 {
			res.Duplicate = true
			return touchStatus(tx, rep.CP, markHeard)
		}
		var others int64
		if err := tx.Model(&ReceivedBatch{}).
			Where("cp_code = ? AND seq = ? AND id <> ?", rep.CP, rep.Seq, rb.ID).
			Count(&others).Error; err != nil {
			return err
		}
		res.SeqReused = others > 0

		seq := rep.Seq
		events := make([]ReceivedEntry, len(rep.Entries))
		for i, e := range rep.Entries {
			events[i] = ReceivedEntry{
				CPCode: rep.CP, Bib: e.Bib, TimeIn: resolve(i), IsVoid: e.Void,
				BatchSeq: &seq, SourceCall: source, ReceivedAt: recv,
			}
		}
		if err := tx.Create(&events).Error; err != nil {
			return err
		}
		res.Entries = len(events)

		return touchStatus(tx, rep.CP, func(st *CheckpointStatus) {
			markHeard(st)
			if plausibleSeq(st, rep.Seq) {
				st.MaxSeq = max(st.MaxSeq, rep.Seq)
			}
			st.BatchesReceived++
			if res.SeqReused {
				st.SeqReuseCount++
			}
		})
	})
	if err != nil {
		return IngestResult{}, err
	}
	return res, nil
}

// RecordHeartbeat stores a checkpoint heartbeat. raceNow is HQ's race
// clock, used to compute the checkpoint's clock skew; when HQ's own
// clock is unsynced (raceSynced false) the skew is unknown and stored
// as nil rather than blaming every checkpoint for HQ's error. A lastseq
// beyond maxHeartbeatSeqJump of anything seen is ignored.
func (s *Store) RecordHeartbeat(ctx context.Context, hb *Heartbeat, source string, receivedAt, raceNow time.Time, raceSynced bool) error {
	recv := normTime(receivedAt)
	var skew *int
	if raceSynced {
		ref := normTime(raceNow)
		sec := int(hb.Time.Resolve(ref).Sub(ref) / time.Second)
		skew = &sec
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return touchStatus(tx, hb.CP, func(st *CheckpointStatus) {
			heard(st, source, recv)
			st.HeartbeatAt = &recv
			if plausibleSeq(st, hb.LastSeq) {
				st.HeartbeatLastSeq = hb.LastSeq
			}
			st.ClockSkewSec = skew
			switch {
			case hb.Closed && st.ClosedAt == nil:
				st.ClosedAt = &recv
			case !hb.Closed:
				st.ClosedAt = nil
			}
		})
	})
}

// plausibleSeq reports whether seq is within maxHeartbeatSeqJump of the
// highest seq this checkpoint has reported.
func plausibleSeq(st *CheckpointStatus, seq uint32) bool {
	return uint64(seq) <= uint64(max(st.MaxSeq, st.HeartbeatLastSeq))+maxHeartbeatSeqJump
}

// LogHQLocal records a bib logged on HQ's own keypad (e.g. START, FIN).
// It goes straight into the event log; nothing is transmitted.
func (s *Store) LogHQLocal(ctx context.Context, cp string, bib Bib, timeIn time.Time) (*ReceivedEntry, error) {
	if !wire.ValidCheckpointCode(cp) {
		return nil, fmt.Errorf("%w: checkpoint code %q", ErrInvalidInput, cp)
	}
	if !bib.Valid() {
		return nil, fmt.Errorf("%w: bib %d", ErrInvalidInput, bib)
	}
	e := &ReceivedEntry{CPCode: cp, Bib: bib, TimeIn: normTime(timeIn), ReceivedAt: normTime(s.now())}
	if err := s.db.WithContext(ctx).Create(e).Error; err != nil {
		return nil, err
	}
	return e, nil
}

// VoidHQLocal cancels HQ-local entry id by appending a void event.
// Remote entries can only be voided by the checkpoint that logged them.
func (s *Store) VoidHQLocal(ctx context.Context, id uint, now time.Time) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e ReceivedEntry
		if err := tx.First(&e, id).Error; err != nil {
			return mapDBError(err)
		}
		if !e.Local() || e.IsVoid {
			return ErrNotFound
		}
		e.TimeIn = normTime(e.TimeIn)
		net, err := netCount(tx, e.CPCode, e.Bib, e.TimeIn)
		if err != nil {
			return err
		}
		if net <= 0 {
			return ErrAlreadyVoided
		}
		return tx.Create(&ReceivedEntry{
			CPCode: e.CPCode, Bib: e.Bib, TimeIn: e.TimeIn, IsVoid: true, ReceivedAt: normTime(now),
		}).Error
	})
}

// GetReceivedEntry loads one HQ event by id.
func (s *Store) GetReceivedEntry(ctx context.Context, id uint) (*ReceivedEntry, error) {
	var e ReceivedEntry
	if err := s.db.WithContext(ctx).First(&e, id).Error; err != nil {
		return nil, mapDBError(err)
	}
	e.TimeIn, e.ReceivedAt = normTime(e.TimeIn), normTime(e.ReceivedAt)
	return &e, nil
}

// AppendEvents adds entry/void events to HQ's log in one transaction.
func (s *Store) AppendEvents(ctx context.Context, events []ReceivedEntry) error {
	if len(events) == 0 {
		return nil
	}
	for i := range events {
		events[i].TimeIn = normTime(events[i].TimeIn)
		events[i].ReceivedAt = normTime(events[i].ReceivedAt)
	}
	return s.db.WithContext(ctx).Create(&events).Error
}

// PassageKey identifies one runner passage: checkpoint, bib, and the
// whole-second UTC time stamped at the checkpoint.
type PassageKey struct {
	CP   string
	Bib  Bib
	Unix int64
}

// NetCounts returns entries minus voids per passage at the given
// checkpoints (all if cps is empty), including passages that net to
// zero or below.
func (s *Store) NetCounts(ctx context.Context, cps []string) (map[PassageKey]int, error) {
	return netCounts(s.db.WithContext(ctx), cps)
}

func netCounts(db *gorm.DB, cps []string) (map[PassageKey]int, error) {
	q := db.Model(&ReceivedEntry{})
	if len(cps) > 0 {
		q = q.Where("cp_code IN ?", cps)
	}
	var rows []ReceivedEntry
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := map[PassageKey]int{}
	for _, r := range rows {
		k := PassageKey{r.CPCode, r.Bib, normTime(r.TimeIn).Unix()}
		if r.IsVoid {
			out[k]--
		} else {
			out[k]++
		}
	}
	return out, nil
}

// ReconcileResult reports what ReconcilePassages changed.
type ReconcileResult struct {
	Added    int // entry events added
	Voided   int // void events added
	Deferred int // passages skipped because HQ's net count is negative
}

// ReconcilePassages brings HQ's net count for each passage in want to
// want's value (clamped at 0) by appending entry or void events with
// the given source, all in one transaction so a concurrent radio ingest
// or a second import can't interleave and double-count. A passage whose
// HQ net count is negative is skipped (Deferred): a void arrived but the
// batch with its entry is still missing, and adding an entry now would
// become a phantom runner when that batch lands.
func (s *Store) ReconcilePassages(ctx context.Context, want map[PassageKey]int, source string, now time.Time) (ReconcileResult, error) {
	var res ReconcileResult
	cps := map[string]bool{}
	for k := range want {
		cps[k.CP] = true
	}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		have, err := netCounts(tx, keysOf(cps))
		if err != nil {
			return err
		}
		var events []ReceivedEntry
		for _, k := range sortedKeys(want) {
			if have[k] < 0 {
				res.Deferred++
				continue
			}
			diff := max(want[k], 0) - have[k]
			for ; diff > 0; diff-- {
				events = append(events, passageEvent(k, false, source, now))
				res.Added++
			}
			for ; diff < 0; diff++ {
				events = append(events, passageEvent(k, true, source, now))
				res.Voided++
			}
		}
		if len(events) == 0 {
			return nil
		}
		return tx.Create(&events).Error
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	return res, nil
}

func passageEvent(k PassageKey, void bool, source string, now time.Time) ReceivedEntry {
	return ReceivedEntry{CPCode: k.CP, Bib: k.Bib, TimeIn: time.Unix(k.Unix, 0).UTC(),
		IsVoid: void, SourceCall: source, ReceivedAt: normTime(now)}
}

func netCount(tx *gorm.DB, cp string, bib Bib, timeIn time.Time) (int, error) {
	var net int64
	err := tx.Model(&ReceivedEntry{}).
		Where("cp_code = ? AND bib = ? AND time_in = ?", cp, bib, timeIn).
		Select("COALESCE(SUM(CASE WHEN is_void THEN -1 ELSE 1 END), 0)").
		Scan(&net).Error
	return int(net), err
}

// EffectiveEntries nets the event log and returns surviving passages
// ordered by time, then checkpoint, then bib. The log is small (a few
// thousand events for a 500-runner race), so netting happens in Go
// rather than in a GROUP BY whose aggregates would lose column types.
func (s *Store) EffectiveEntries(ctx context.Context, f EntryFilter) ([]EffectiveEntry, error) {
	q := s.db.WithContext(ctx).Model(&ReceivedEntry{})
	if f.Bib != nil {
		q = q.Where("bib = ?", *f.Bib)
	}
	if f.CPCode != "" {
		q = q.Where("cp_code = ?", f.CPCode)
	}
	var rows []ReceivedEntry
	if err := q.Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}

	type key struct {
		cp   string
		bib  Bib
		unix int64
	}
	byKey := map[key]*EffectiveEntry{}
	var order []key
	for _, r := range rows {
		k := key{r.CPCode, r.Bib, normTime(r.TimeIn).Unix()}
		ee, ok := byKey[k]
		if !ok {
			ee = &EffectiveEntry{CPCode: r.CPCode, Bib: r.Bib, TimeIn: normTime(r.TimeIn)}
			byKey[k] = ee
			order = append(order, k)
		}
		if r.IsVoid {
			ee.Count--
			continue
		}
		if ee.ReceivedAt.IsZero() {
			ee.SourceCall, ee.ReceivedAt = r.SourceCall, normTime(r.ReceivedAt)
		}
		ee.Count++
	}

	var out []EffectiveEntry
	for _, k := range order {
		if ee := byKey[k]; ee.Count > 0 {
			out = append(out, *ee)
		}
	}
	slices.SortStableFunc(out, func(a, b EffectiveEntry) int {
		return cmp.Or(a.TimeIn.Compare(b.TimeIn), cmp.Compare(a.CPCode, b.CPCode), cmp.Compare(a.Bib, b.Bib))
	})
	return out, nil
}

// MissingSeqs returns up to limit batch seqs HQ expects from cp but
// hasn't received, ascending. The expected range runs to the higher of
// the largest seq received and the last heartbeat's lastseq (which
// exposes a lost tail). Work is bounded by limit plus the number of
// received batches, so a bogus huge lastseq can't stall HQ.
func (s *Store) MissingSeqs(ctx context.Context, cp string, limit int) ([]uint32, error) {
	var st CheckpointStatus
	err := s.db.WithContext(ctx).Where("cp_code = ?", cp).First(&st).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var have []uint32
	if err := s.db.WithContext(ctx).Model(&ReceivedBatch{}).Where("cp_code = ?", cp).
		Distinct().Order("seq ASC").Pluck("seq", &have).Error; err != nil {
		return nil, err
	}
	expected := uint64(max(st.MaxSeq, st.HeartbeatLastSeq))
	var missing []uint32
	next := 0 // index into have
	for seq := uint64(1); seq <= expected && len(missing) < limit; seq++ {
		if next < len(have) && uint64(have[next]) == seq {
			next++
			continue
		}
		missing = append(missing, uint32(seq))
	}
	return missing, nil
}

// ListStatuses returns every checkpoint HQ has heard from, by code.
func (s *Store) ListStatuses(ctx context.Context) ([]CheckpointStatus, error) {
	var out []CheckpointStatus
	if err := s.db.WithContext(ctx).Order("cp_code ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		out[i].LastHeardAt = normTimePtr(out[i].LastHeardAt)
		out[i].HeartbeatAt = normTimePtr(out[i].HeartbeatAt)
	}
	return out, nil
}

// touchStatus loads (or starts) cp's status row, applies fn, and saves.
// Callers run it inside a transaction.
func touchStatus(tx *gorm.DB, cp string, fn func(*CheckpointStatus)) error {
	st := CheckpointStatus{CPCode: cp}
	if err := tx.Where("cp_code = ?", cp).First(&st).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	fn(&st)
	return tx.Save(&st).Error
}

// heard records the latest packet from a checkpoint. The most recent
// packet always wins, even if its timestamp is earlier: the race clock
// can step backwards (a late browser sync), and gap requests must go to
// the callsign the checkpoint is using now.
func heard(st *CheckpointStatus, source string, at time.Time) {
	st.LastHeardAt = &at
	st.LastSourceCall = source
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func sortedKeys(m map[PassageKey]int) []PassageKey {
	out := make([]PassageKey, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b PassageKey) int {
		return cmp.Or(cmp.Compare(a.Unix, b.Unix), cmp.Compare(a.CP, b.CP), cmp.Compare(a.Bib, b.Bib))
	})
	return out
}

// HQLocalEntry is one HQ keypad entry for the keypad's list.
type HQLocalEntry struct {
	ID     uint
	CP     string
	Bib    Bib
	TimeIn time.Time
	Voided bool
}

// ListHQLocal lists HQ keypad entries, newest first. A void at HQ cancels
// a passage, not a row, so when one passage was logged twice (a double
// tap) and voided once, the newest copy is the one shown as voided.
func (s *Store) ListHQLocal(ctx context.Context, limit int) ([]HQLocalEntry, error) {
	var events []ReceivedEntry
	if err := s.db.WithContext(ctx).
		Where("batch_seq IS NULL AND source_call = ''").
		Order("id ASC").Find(&events).Error; err != nil {
		return nil, err
	}
	voids := map[PassageKey]int{}
	for _, e := range events {
		if e.IsVoid {
			voids[PassageKey{e.CPCode, e.Bib, normTime(e.TimeIn).Unix()}]++
		}
	}
	var out []HQLocalEntry
	for i := len(events) - 1; i >= 0; i-- { // newest first
		e := events[i]
		if e.IsVoid {
			continue
		}
		k := PassageKey{e.CPCode, e.Bib, normTime(e.TimeIn).Unix()}
		voided := voids[k] > 0
		if voided {
			voids[k]--
		}
		out = append(out, HQLocalEntry{ID: e.ID, CP: e.CPCode, Bib: e.Bib, TimeIn: normTime(e.TimeIn), Voided: voided})
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
