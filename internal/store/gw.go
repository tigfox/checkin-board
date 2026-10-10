package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"checkin-board/internal/wire"
)

// graywolf bookkeeping: which graywolf message rows are ours, where the
// inbox reader is, undecodable reports, and peers' pre-race prefs.

// Kinds of graywolf message rows the app records (gw_rows.kind).
const (
	GWRowBatch     = "batch"     // outbound RC1 R
	GWRowHeartbeat = "heartbeat" // outbound RC1 H
	GWRowGap       = "gap"       // outbound RC1 G
	GWRowProbe     = "probe"     // outbound RC1 P
	GWRowReply     = "reply"     // outbound RC1 Q
	GWRowInbound   = "inbound"   // inbound RC1 the app processed
)

var validGWRowKinds = map[string]bool{
	GWRowBatch: true, GWRowHeartbeat: true, GWRowGap: true,
	GWRowProbe: true, GWRowReply: true, GWRowInbound: true,
}

// Bounds on what is kept of an undecodable report.
const (
	maxBadReportText   = 256
	maxBadReportFrom   = 16
	maxBadReportReason = 256
)

// GWRow is one graywolf message row the app sent or ingested.
type GWRow struct {
	GWMessageID uint64     `gorm:"column:gw_message_id;primaryKey"`
	Kind        string     `gorm:"column:kind"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	DeletedAt   *time.Time `gorm:"column:deleted_at"`
	// BatchID is the checkpoint batch a batch row is a copy of; cleared
	// when a gap request makes earlier copies void.
	BatchID *uint `gorm:"column:batch_id"`
}

func (GWRow) TableName() string { return "gw_rows" }

// BadReport is RC1 text graywolf ACKed that the app couldn't decode.
type BadReport struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	GWMessageID uint64    `gorm:"column:gw_message_id"`
	FromCall    string    `gorm:"column:from_call"`
	Text        string    `gorm:"column:text"`
	Error       string    `gorm:"column:error"`
	ReceivedAt  time.Time `gorm:"column:received_at"`
}

func (BadReport) TableName() string { return "bad_reports" }

// PeerPrefs is a race peer's graywolf conversation prefs as they were
// before the app turned graywolf's retries off (spec 3.3).
type PeerPrefs struct {
	Callsign   string    `gorm:"column:callsign;primaryKey"`
	SendPath   string    `gorm:"column:send_path"`
	WaitForAck bool      `gorm:"column:wait_for_ack"`
	SavedAt    time.Time `gorm:"column:saved_at"`
}

func (PeerPrefs) TableName() string { return "peer_prefs_backup" }

type inboxState struct {
	ID        uint       `gorm:"column:id;primaryKey"`
	Cursor    string     `gorm:"column:cursor"`
	Since     *time.Time `gorm:"column:since"`
	UpdatedAt time.Time  `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (inboxState) TableName() string { return "inbox_state" }

func recordGWRow(tx *gorm.DB, gwID uint64, kind string, now time.Time) error {
	_, err := insertGWRow(tx, gwID, kind, now)
	return err
}

// insertGWRow inserts the row unless it exists; it reports whether it
// was new.
func insertGWRow(tx *gorm.DB, gwID uint64, kind string, now time.Time) (bool, error) {
	if gwID == 0 {
		return false, fmt.Errorf("%w: graywolf message id must be non-zero", ErrInvalidInput)
	}
	if !validGWRowKinds[kind] {
		return false, fmt.Errorf("%w: graywolf row kind %q", ErrInvalidInput, kind)
	}
	res := tx.Clauses(clause.OnConflict{DoNothing: true}).
		Create(&GWRow{GWMessageID: gwID, Kind: kind, CreatedAt: normTime(now)})
	return res.RowsAffected == 1, res.Error
}

// RecordGWRow records a graywolf message row the app sent or processed.
// It reports whether the row was new; recording a known row is a no-op.
func (s *Store) RecordGWRow(ctx context.Context, gwID uint64, kind string) (bool, error) {
	return insertGWRow(s.db.WithContext(ctx), gwID, kind, s.now())
}

// KnownGWRow reports whether the app has already recorded gwID, so the
// inbox reader can skip rows it has processed.
func (s *Store) KnownGWRow(ctx context.Context, gwID uint64) (bool, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&GWRow{}).Where("gw_message_id = ?", gwID).Count(&n).Error
	return n > 0, err
}

// ListGWRows returns the recorded rows not yet deleted from graywolf,
// oldest first: what post-race cleanup may delete (spec 4.7.3).
func (s *Store) ListGWRows(ctx context.Context) ([]GWRow, error) {
	var out []GWRow
	if err := s.db.WithContext(ctx).Where("deleted_at IS NULL").
		Order("gw_message_id ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		out[i].CreatedAt = normTime(out[i].CreatedAt)
	}
	return out, nil
}

// MarkGWRowDeleted records that cleanup deleted gwID from graywolf.
func (s *Store) MarkGWRowDeleted(ctx context.Context, gwID uint64, at time.Time) error {
	return rowsOrNotFound(s.db.WithContext(ctx).Model(&GWRow{}).
		Where("gw_message_id = ?", gwID).Update("deleted_at", normTime(at)))
}

// InboxCursor returns the saved graywolf inbox cursor ("" = start).
func (s *Store) InboxCursor(ctx context.Context) (string, error) {
	var st inboxState
	err := s.db.WithContext(ctx).First(&st, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return st.Cursor, err
}

// SaveInboxCursor persists the inbox cursor (leaving the starting
// point alone). Call it only after every row before the cursor has been
// processed and committed.
func (s *Store) SaveInboxCursor(ctx context.Context, cursor string) error {
	row := inboxState{ID: 1, Cursor: cursor, UpdatedAt: normTime(s.now())}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"cursor", "updated_at"}),
	}).Create(&row).Error
}

// InboxSince returns the reader's saved starting point (zero if none).
func (s *Store) InboxSince(ctx context.Context) (time.Time, error) {
	var st inboxState
	err := s.db.WithContext(ctx).First(&st, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) || (err == nil && st.Since == nil) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return normTime(*st.Since), nil
}

// EnsureInboxSince saves t as the reader's starting point unless one is
// already saved, so it is set once, on a node's first run.
func (s *Store) EnsureInboxSince(ctx context.Context, t time.Time) error {
	since := normTime(t)
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).
			Create(&inboxState{ID: 1, Since: &since, UpdatedAt: normTime(s.now())}).Error; err != nil {
			return err
		}
		return tx.Model(&inboxState{}).Where("id = 1 AND since IS NULL").Update("since", since).Error
	})
}

// SetInboxSince replaces the starting point and clears the cursor, so
// the reader re-reads graywolf's messages from t (an admin recovery
// action, e.g. after a wiped database; spec 4.7). Rows already recorded
// are skipped, so re-reading is safe.
func (s *Store) SetInboxSince(ctx context.Context, t time.Time) error {
	since := normTime(t)
	return s.db.WithContext(ctx).Save(&inboxState{ID: 1, Since: &since, UpdatedAt: normTime(s.now())}).Error
}

// RecordBadReport keeps an undecodable RC1 text and records its
// graywolf row (so the inbox reader skips it and cleanup deletes it). It
// counts against checkpoint cp only when cp is already known (defined in
// checkpoints, or already reporting): a garbage or spoofed text must not
// create a phantom checkpoint. Recording the same row twice is a no-op.
// Strings are truncated, on UTF-8 boundaries, to bounded lengths.
func (s *Store) RecordBadReport(ctx context.Context, gwID uint64, from, cp, text, reason string, at time.Time) error {
	if gwID == 0 {
		return fmt.Errorf("%w: graywolf message id must be non-zero", ErrInvalidInput)
	}
	row := BadReport{
		GWMessageID: gwID,
		FromCall:    truncateUTF8(from, maxBadReportFrom),
		Text:        truncateUTF8(text, maxBadReportText),
		Error:       truncateUTF8(reason, maxBadReportReason),
		ReceivedAt:  normTime(at),
	}
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := recordGWRow(tx, gwID, GWRowInbound, at); err != nil {
			return err
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil || res.RowsAffected == 0 {
			return res.Error
		}
		known, err := knownCheckpoint(tx, cp)
		if err != nil || !known {
			return err
		}
		return touchStatus(tx, cp, func(st *CheckpointStatus) { st.BadReports++ })
	})
}

// knownCheckpoint reports whether cp is defined at HQ or has reported.
func knownCheckpoint(tx *gorm.DB, cp string) (bool, error) {
	if !wire.ValidCheckpointCode(cp) {
		return false, nil
	}
	var n int64
	err := tx.Raw(`SELECT (SELECT COUNT(*) FROM checkpoints WHERE code = ?) +
		(SELECT COUNT(*) FROM cp_status WHERE cp_code = ?)`, cp, cp).Scan(&n).Error
	return n > 0, err
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune and
// replaces invalid UTF-8, so the stored text always encodes as JSON.
func truncateUTF8(s string, n int) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ListBadReports returns the newest limit undecodable reports.
func (s *Store) ListBadReports(ctx context.Context, limit int) ([]BadReport, error) {
	var out []BadReport
	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		out[i].ReceivedAt = normTime(out[i].ReceivedAt)
	}
	return out, nil
}

// SavePeerPrefs stores a peer's original graywolf prefs unless a backup
// already exists: the first backup is the true pre-race state, and a
// restart must not overwrite it with the app's own wait_for_ack=false.
// It reports whether a backup was written.
func (s *Store) SavePeerPrefs(ctx context.Context, p PeerPrefs) (bool, error) {
	if !ValidStationCall(p.Callsign) {
		return false, fmt.Errorf("%w: callsign %q", ErrInvalidInput, p.Callsign)
	}
	row := PeerPrefs{Callsign: p.Callsign, SendPath: p.SendPath, WaitForAck: p.WaitForAck, SavedAt: normTime(s.now())}
	res := s.db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
	return res.RowsAffected == 1, res.Error
}

// ListPeerPrefs returns every saved backup, by callsign.
func (s *Store) ListPeerPrefs(ctx context.Context) ([]PeerPrefs, error) {
	var out []PeerPrefs
	if err := s.db.WithContext(ctx).Order("callsign ASC").Find(&out).Error; err != nil {
		return nil, err
	}
	for i := range out {
		out[i].SavedAt = normTime(out[i].SavedAt)
	}
	return out, nil
}

// DeletePeerPrefs removes a backup once the prefs have been restored.
func (s *Store) DeletePeerPrefs(ctx context.Context, callsign string) error {
	return rowsOrNotFound(s.db.WithContext(ctx).Where("callsign = ?", callsign).Delete(&PeerPrefs{}))
}
