// Package store is checkin-board's persistence layer: one SQLite file
// (checkin-board.db) holding settings, the checkpoint outbox, HQ's event
// log, reference data and graywolf bookkeeping. Schema source of truth
// is migrations/*.sql; the models are never AutoMigrated.
//
// Most of this package is ported from graywolf pkg/race (our own code),
// with graywolf's message-store coupling replaced by graywolf row ids
// (spec section 3.1). Design: docs/specs/2026-10-05-race-checkpoint-design.md.
package store

import (
	"time"

	"gorm.io/gorm"

	"checkin-board/internal/wire"
)

// The store speaks the wire package's value types.
type (
	Bib       = wire.Bib
	Entry     = wire.Entry
	Report    = wire.Report
	Heartbeat = wire.Heartbeat
)

// Store is safe for concurrent use; writes are serialized by the single
// SQLite connection.
type Store struct {
	db  *gorm.DB
	now func() time.Time // row-creation timestamps; injectable for tests
}

// SetClock overrides the clock used for row-creation timestamps (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }
