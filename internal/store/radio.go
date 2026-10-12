package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
)

// RadioMessage is a message row of the UV-K5 radio backend (phase 17b),
// shaped like graywolf's: the radio itself keeps no history.
type RadioMessage struct {
	ID            uint64     `gorm:"column:id;primaryKey"`
	Direction     string     `gorm:"column:direction"` // "in" or "out"
	Status        string     `gorm:"column:status"`    // graywolf's status names
	FromCall      string     `gorm:"column:from_call"`
	ToCall        string     `gorm:"column:to_call"`
	PeerCall      string     `gorm:"column:peer_call"`
	Text          string     `gorm:"column:text"`
	MsgID         string     `gorm:"column:msg_id"` // the radio's line number (out) or the sender's id (in)
	Via           string     `gorm:"column:via"`
	Unread        bool       `gorm:"column:unread"`
	FailureReason string     `gorm:"column:failure_reason"`
	ClientID      string     `gorm:"column:client_id"`
	CreatedAt     time.Time  `gorm:"column:created_at;autoCreateTime:false"`
	SentAt        *time.Time `gorm:"column:sent_at"`
	ReceivedAt    *time.Time `gorm:"column:received_at"`
	AckedAt       *time.Time `gorm:"column:acked_at"`
	UpdSeq        uint64     `gorm:"column:upd_seq"`
}

func (RadioMessage) TableName() string { return "radio_messages" }

func nextRadioSeq(tx *gorm.DB) (uint64, error) {
	var max *uint64
	if err := tx.Model(&RadioMessage{}).Select("MAX(upd_seq)").Scan(&max).Error; err != nil {
		return 0, err
	}
	if max == nil {
		return 1, nil
	}
	return *max + 1, nil
}

// RadioInsert stores a new row, setting its ID, creation time (if zero)
// and change sequence.
func (s *Store) RadioInsert(ctx context.Context, m *RadioMessage) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		seq, err := nextRadioSeq(tx)
		if err != nil {
			return err
		}
		m.ID, m.UpdSeq = 0, seq
		if m.CreatedAt.IsZero() {
			m.CreatedAt = s.now()
		}
		m.CreatedAt = normTime(m.CreatedAt)
		return tx.Create(m).Error
	})
}

// RadioUpdate applies change to row id in one transaction; when change
// reports a change, the row moves to the end of the change feed.
func (s *Store) RadioUpdate(ctx context.Context, id uint64, change func(*RadioMessage) bool) (RadioMessage, error) {
	var out RadioMessage
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Take(&out, id).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrNotFound
			}
			return err
		}
		if !change(&out) {
			return nil
		}
		seq, err := nextRadioSeq(tx)
		if err != nil {
			return err
		}
		out.UpdSeq = seq
		return tx.Save(&out).Error
	})
	return out, err
}

// RadioGet returns row id.
func (s *Store) RadioGet(ctx context.Context, id uint64) (RadioMessage, error) {
	var m RadioMessage
	err := s.db.WithContext(ctx).Take(&m, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

// RadioChanges returns rows changed after sequence after, in change
// order, at most limit (0: all).
func (s *Store) RadioChanges(ctx context.Context, after uint64, limit int) ([]RadioMessage, error) {
	q := s.db.WithContext(ctx).Where("upd_seq > ?", after).Order("upd_seq")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var out []RadioMessage
	return out, q.Find(&out).Error
}

// RadioOutbound returns outbound rows in any of statuses, oldest first.
func (s *Store) RadioOutbound(ctx context.Context, statuses ...string) ([]RadioMessage, error) {
	var out []RadioMessage
	return out, s.db.WithContext(ctx).Where("direction = 'out' AND status IN ?", statuses).Order("id").Find(&out).Error
}

// RadioMatchAck finds the outbound row to peer with message id msgID
// that's still waiting for an ack (in one of statuses), sent since
// since: the newest such row, since line numbers wrap at 99.
func (s *Store) RadioMatchAck(ctx context.Context, peer, msgID string, since time.Time, statuses ...string) (RadioMessage, error) {
	var m RadioMessage
	err := s.db.WithContext(ctx).
		Where("direction = 'out' AND peer_call = ? COLLATE NOCASE AND msg_id = ? AND status IN ? AND created_at >= ?",
			peer, msgID, statuses, normTime(since)).
		Order("id DESC").Take(&m).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return m, ErrNotFound
	}
	return m, err
}

// RadioDelete removes row id.
func (s *Store) RadioDelete(ctx context.Context, id uint64) error {
	res := s.db.WithContext(ctx).Delete(&RadioMessage{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}
