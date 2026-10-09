package store

import "time"

// Models for migrations/*.sql. Columns are tagged explicitly because
// gorm's default naming mangles initialisms like "HQ" and "CP". Bool
// columns carry no gorm default tag on purpose: a default would make
// gorm drop an explicit false from INSERTs. created_at / updated_at
// disable gorm's automatic timestamps: the store stamps them from its
// own (injectable) clock, normalized to whole seconds.

// Local entry states (checkpoint side).
const (
	EntryQueued    = "queued"    // logged, not yet in a batch
	EntrySent      = "sent"      // in a batch awaiting HQ's ACK
	EntryConfirmed = "confirmed" // batch ACKed by HQ
)

// Batch states (checkpoint side).
const (
	BatchPending  = "pending" // to be (re)transmitted until ACKed
	BatchAcked    = "acked"
	BatchRejected = "rejected" // REJ, or graywolf refused it (400); parked until a gap request revives it
)

// LocalEntry is one bib logged at this checkpoint. A row with VoidOf
// set is a correction cancelling the entry it points at; it carries the
// same bib and time so it can travel as a "-bib/ss" wire entry.
type LocalEntry struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	CPCode      string    `gorm:"column:cp_code"`
	Bib         Bib       `gorm:"column:bib"`
	TimeIn      time.Time `gorm:"column:time_in"`
	ClockSynced bool      `gorm:"column:clock_synced"`
	State       string    `gorm:"column:state"`
	VoidOf      *uint     `gorm:"column:void_of"`
	BatchID     *uint     `gorm:"column:batch_id"`
	CreatedAt   time.Time `gorm:"column:created_at;autoCreateTime:false"`
}

func (LocalEntry) TableName() string { return "local_entries" }

// Batch is one RC1 report in the checkpoint's outbox. Once first sent
// it is bound to one graywolf message row (GWMessageID), and every
// retransmit is a resend of that row (spec 3.1).
type Batch struct {
	ID          uint       `gorm:"column:id;primaryKey"`
	CPCode      string     `gorm:"column:cp_code"`
	Seq         uint32     `gorm:"column:seq"`
	Text        string     `gorm:"column:text"`
	State       string     `gorm:"column:state"`
	GWMessageID *uint64    `gorm:"column:gw_message_id"`
	GWMsgID     string     `gorm:"column:gw_msg_id"`
	ClientID    string     `gorm:"column:client_id"`
	Attempts    int        `gorm:"column:attempts"`
	LastTxAt    *time.Time `gorm:"column:last_tx_at"`
	NextTxAt    time.Time  `gorm:"column:next_tx_at"`
	AckedAt     *time.Time `gorm:"column:acked_at"`
	CreatedAt   time.Time  `gorm:"column:created_at;autoCreateTime:false"`
}

func (Batch) TableName() string { return "batches" }

// ReceivedBatch records one distinct batch text HQ has ingested. The
// (cp_code, seq, text) key is the batch-level dedup.
type ReceivedBatch struct {
	ID          uint      `gorm:"column:id;primaryKey"`
	CPCode      string    `gorm:"column:cp_code"`
	Seq         uint32    `gorm:"column:seq"`
	Text        string    `gorm:"column:text"`
	SourceCall  string    `gorm:"column:source_call"`
	GWMessageID *uint64   `gorm:"column:gw_message_id"`
	ReceivedAt  time.Time `gorm:"column:received_at"`
}

func (ReceivedBatch) TableName() string { return "received_batches" }

// ReceivedEntry is one event in HQ's append-only entry log: an entry
// (IsVoid=false) or a void (IsVoid=true). Local HQ entries have no
// BatchSeq and an empty SourceCall.
type ReceivedEntry struct {
	ID         uint      `gorm:"column:id;primaryKey"`
	CPCode     string    `gorm:"column:cp_code"`
	Bib        Bib       `gorm:"column:bib"`
	TimeIn     time.Time `gorm:"column:time_in"`
	IsVoid     bool      `gorm:"column:is_void"`
	BatchSeq   *uint32   `gorm:"column:batch_seq"`
	SourceCall string    `gorm:"column:source_call"`
	ReceivedAt time.Time `gorm:"column:received_at"`
}

func (ReceivedEntry) TableName() string { return "received_entries" }

// Local reports whether the entry was logged on HQ's own keypad.
func (e ReceivedEntry) Local() bool { return e.BatchSeq == nil && e.SourceCall == "" }

// CheckpointStatus is HQ's health view of one checkpoint.
type CheckpointStatus struct {
	CPCode           string     `gorm:"column:cp_code;primaryKey"`
	LastHeardAt      *time.Time `gorm:"column:last_heard_at"`
	LastSourceCall   string     `gorm:"column:last_source_call"`
	HeartbeatAt      *time.Time `gorm:"column:heartbeat_at"`
	HeartbeatLastSeq uint32     `gorm:"column:heartbeat_last_seq"`
	// ClockSkewSec is checkpoint clock minus HQ race clock at the last
	// heartbeat (includes a few seconds of RF latency). Nil until heard.
	ClockSkewSec *int `gorm:"column:clock_skew_sec"`
	// ClosedAt is when the checkpoint's heartbeats first said it had
	// closed (4.7); nil while open.
	ClosedAt        *time.Time `gorm:"column:closed_at"`
	MaxSeq          uint32     `gorm:"column:max_seq"`
	BatchesReceived int        `gorm:"column:batches_received"`
	// SeqReuseCount counts batches that reused a known seq with new
	// text, which usually means the checkpoint's DB was reset.
	SeqReuseCount int `gorm:"column:seq_reuse_count"`
	// BadReports counts RC1 texts from this checkpoint that didn't decode.
	BadReports int `gorm:"column:bad_reports"`
}

func (CheckpointStatus) TableName() string { return "cp_status" }

// Checkpoint is HQ reference data: one point on the course.
type Checkpoint struct {
	ID           uint      `gorm:"column:id;primaryKey"`
	Code         string    `gorm:"column:code"`
	Name         string    `gorm:"column:name"`
	CourseOrder  int       `gorm:"column:course_order"`
	ExpectedCall string    `gorm:"column:expected_call"`
	CreatedAt    time.Time `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt    time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (Checkpoint) TableName() string { return "checkpoints" }

// Runner is one roster entry: a valid bib and an optional race-relevant
// category (distance, course, wave). No personal data (name, gender,
// age, contact) is ever stored; the roster import discards it.
type Runner struct {
	ID        uint      `gorm:"column:id;primaryKey"`
	Bib       Bib       `gorm:"column:bib"`
	Category  string    `gorm:"column:category"`
	CreatedAt time.Time `gorm:"column:created_at;autoCreateTime:false"`
	UpdatedAt time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (Runner) TableName() string { return "runners" }

// normTime is the storage form of every timestamp: UTC, whole seconds.
// One form keeps SQLite's string comparisons and equality lookups exact.
func normTime(t time.Time) time.Time { return t.UTC().Truncate(time.Second) }

// normTimePtr applies normTime to a nullable column after a read.
func normTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	n := normTime(*t)
	return &n
}
