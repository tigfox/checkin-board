package graywolf

import "time"

// Message status values returned in Message.Status. Mirrors graywolf's
// published status strings (see graywolf 0.14.14 /api/messages).
const (
	StatusQueued      = "queued"
	StatusTxSubmitted = "tx_submitted"
	StatusSentRF      = "sent_rf"
	StatusSentIS      = "sent_is"
	StatusAwaitingAck = "awaiting_ack"
	StatusAcked       = "acked"
	StatusRejected    = "rejected"
	StatusTimeout     = "timeout"
	StatusBroadcast   = "sent"
	StatusFailed      = "failed"
	StatusReceived    = "received"
)

// Folder values for ListParams.Folder.
const (
	FolderInbox = "inbox"
	FolderSent  = "sent"
	FolderAll   = "all"
)

// Thread kinds used in conversation-prefs paths.
const (
	ThreadKindDM       = "dm"
	ThreadKindTactical = "tactical"
)

// SSE event types emitted on /api/messages/events.
const (
	EventReceived     = "message.received"
	EventAcked        = "message.acked"
	EventRejected     = "message.rejected"
	EventReplyAckRcvd = "message.reply_ack_received"
	EventSentRF       = "message.sent_rf"
	EventSentIS       = "message.sent_is"
	EventFailed       = "message.failed"
	EventDeleted      = "message.deleted"
	EventUpdated      = "message.updated"
)

// Health is the payload of GET /api/health.
type Health struct {
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	Time      time.Time `json:"time"`
}

// Version is the payload of GET /api/version.
type Version struct {
	Version string `json:"version"`
	// Commit is the build's commit stamp; a local ARMv6 build for the Pi
	// Zero ends in "-armv6buf" (deploy/graywolf-armv6).
	Commit   string `json:"commit"`
	Platform string `json:"platform"`
}

// StationConfig is the payload of GET/PUT /api/station/config.
type StationConfig struct {
	Callsign string   `json:"callsign"`
	Disabled []string `json:"disabled,omitempty"`
}

// SendRequest is the body of POST /api/messages. Zero-valued optional
// fields are omitted so graywolf applies its own defaults.
type SendRequest struct {
	To       string `json:"to"`
	Text     string `json:"text"`
	Path     string `json:"path,omitempty"`
	Channel  *int   `json:"channel,omitempty"`
	ClientID string `json:"client_id,omitempty"`
}

// Message is a graywolf message row (dto.MessageResponse). Only the
// fields checkin-board relies on are typed; the rest are ignored.
type Message struct {
	ID            uint64     `json:"id"`
	Direction     string     `json:"direction"`
	Status        string     `json:"status"`
	FromCall      string     `json:"from_call"`
	ToCall        string     `json:"to_call"`
	PeerCall      string     `json:"peer_call"`
	OurCall       string     `json:"our_call"`
	Text          string     `json:"text"`
	MsgID         string     `json:"msg_id"`
	ThreadKind    string     `json:"thread_kind"`
	ThreadKey     string     `json:"thread_key"`
	Source        string     `json:"source"`
	Path          string     `json:"path"`
	Via           string     `json:"via"`
	Channel       int        `json:"channel"`
	Attempts      int        `json:"attempts"`
	Unread        bool       `json:"unread"`
	IsAck         bool       `json:"is_ack"`
	FailureReason string     `json:"failure_reason"`
	ClientID      string     `json:"client_id,omitempty"`
	CreatedAt     *time.Time `json:"created_at,omitempty"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
	ReceivedAt    *time.Time `json:"received_at,omitempty"`
	AckedAt       *time.Time `json:"acked_at,omitempty"`
	NextRetryAt   *time.Time `json:"next_retry_at,omitempty"`
}

// MessageChange wraps a message in list and SSE responses. Message is
// nil for deletions.
type MessageChange struct {
	ID      uint64   `json:"id"`
	Kind    string   `json:"kind"`
	Message *Message `json:"message,omitempty"`
}

// MessagePage is the payload of GET /api/messages.
type MessagePage struct {
	Changes []MessageChange `json:"changes"`
	Cursor  string          `json:"cursor"`
}

// ListParams filters GET /api/messages. Zero values are omitted.
type ListParams struct {
	Folder     string
	Peer       string
	ThreadKind string
	ThreadKey  string
	Since      time.Time
	Cursor     string
	UnreadOnly bool
	Limit      int
}

// MessagePreferences is the payload of GET /api/messages/preferences.
type MessagePreferences struct {
	DefaultPath            string `json:"default_path"`
	FallbackPolicy         string `json:"fallback_policy"`
	MaxMessageTextOverride int    `json:"max_message_text_override"`
	RetentionDays          int    `json:"retention_days"`
	RetryMaxAttempts       int    `json:"retry_max_attempts"`
}

// ConversationPrefs is the per-thread override at
// /api/messages/conversations/{kind}/{key}/prefs.
type ConversationPrefs struct {
	ThreadKind string `json:"thread_kind,omitempty"`
	ThreadKey  string `json:"thread_key,omitempty"`
	SendPath   string `json:"send_path"`
	WaitForAck bool   `json:"wait_for_ack"`
}

// Event is one SSE frame from /api/messages/events.
type Event struct {
	Type   string
	Change MessageChange
}
