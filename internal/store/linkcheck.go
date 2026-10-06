package store

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"

	"checkin-board/internal/wire"
)

// Link-check run states (spec 4.8).
const (
	LinkCheckRequested = "requested" // waiting for the service to pick it up
	LinkCheckRunning   = "running"
	LinkCheckDone      = "done"
	LinkCheckCancelled = "cancelled" // never ran or was abandoned (Error says why)
)

// LinkCheck is one link-check run this node probed.
type LinkCheck struct {
	ID            uint       `gorm:"column:id;primaryKey"`
	Run           int        `gorm:"column:run"`
	PeerCall      string     `gorm:"column:peer_call"`
	StationCode   string     `gorm:"column:station_code"`
	Count         int        `gorm:"column:count"`
	SpacingSec    int        `gorm:"column:spacing_sec"`
	State         string     `gorm:"column:state"`
	Source        string     `gorm:"column:source"`
	RequestedAt   time.Time  `gorm:"column:requested_at"`
	StartedAt     *time.Time `gorm:"column:started_at"`
	FinishedAt    *time.Time `gorm:"column:finished_at"`
	Verdict       string     `gorm:"column:verdict"`
	Uplink        int        `gorm:"column:uplink"`     // probes the responder heard
	RoundTrip     int        `gorm:"column:round_trip"` // probes ACKed
	ReplyReceived bool       `gorm:"column:reply_received"`
	MedianRTTms   *int       `gorm:"column:median_rtt_ms"`
	RemoteLevel   *int       `gorm:"column:remote_level"` // dBFS at the responder
	LocalLevel    *int       `gorm:"column:local_level"`  // dBFS here
	Via           string     `gorm:"column:via"`
	Advice        string     `gorm:"column:advice"`
	Error         string     `gorm:"column:error"`
}

func (LinkCheck) TableName() string { return "link_checks" }

// LinkProbe is one probe of a run.
type LinkProbe struct {
	CheckID     uint       `gorm:"column:check_id;primaryKey"`
	Idx         int        `gorm:"column:idx;primaryKey"`
	GWMessageID *uint64    `gorm:"column:gw_message_id"`
	MsgID       string     `gorm:"column:msg_id"` // the APRS msgid its ACK carries
	SentAt      *time.Time `gorm:"column:sent_at"`
	AckedAt     *time.Time `gorm:"column:acked_at"`
	RTTms       *int       `gorm:"column:rtt_ms"`
}

func (LinkProbe) TableName() string { return "link_probes" }

// LinkResponse is a run this node answered as the responder.
type LinkResponse struct {
	ID            uint       `gorm:"column:id;primaryKey"`
	PeerCall      string     `gorm:"column:peer_call"`
	ProberCode    string     `gorm:"column:prober_code"`
	Run           int        `gorm:"column:run"`
	Total         int        `gorm:"column:total"`
	Heard         string     `gorm:"column:heard"` // sorted indices, "1,2,4"
	FirstHeardAt  time.Time  `gorm:"column:first_heard_at"`
	LastHeardAt   time.Time  `gorm:"column:last_heard_at"`
	Level         *int       `gorm:"column:level"`
	Via           string     `gorm:"column:via"`
	UnknownPeer   bool       `gorm:"column:unknown_peer"`
	ReplyDueAt    *time.Time `gorm:"column:reply_due_at"`
	ReplyAttempts int        `gorm:"column:reply_attempts"`
	ReplyGWID     *uint64    `gorm:"column:reply_gw_id"`
	ReplySentAt   *time.Time `gorm:"column:reply_sent_at"`
	ReplyAckedAt  *time.Time `gorm:"column:reply_acked_at"`
}

func (LinkResponse) TableName() string { return "link_responses" }

// HeardIndices parses Heard.
func (r LinkResponse) HeardIndices() []uint8 {
	var out []uint8
	for _, f := range strings.Split(r.Heard, ",") {
		if n, err := strconv.Atoi(f); err == nil && n > 0 && n <= wire.MaxProbes {
			out = append(out, uint8(n))
		}
	}
	return out
}

const (
	// runReuseGap: a probe this long after the last one of the same
	// (station, run) starts a new run.
	runReuseGap = 30 * time.Minute
	// responseKeep bounds the responder's records (spoofed probes can't
	// grow the table without limit).
	responseKeep = 7 * 24 * time.Hour
)

// ProbeHeard is one probe the responder decoded.
type ProbeHeard struct {
	PeerCall    string
	ProberCode  string
	Run, Total  int
	Idx         int
	At          time.Time
	Level       *int // nil: no reading for this frame
	Via         string
	UnknownPeer bool
}

func validLinkCheck(c LinkCheck) error {
	switch {
	case !ValidStationCall(c.PeerCall):
		return fmt.Errorf("%w: link check peer %q", ErrInvalidInput, c.PeerCall)
	case !wire.ValidCheckpointCode(c.StationCode):
		return fmt.Errorf("%w: link check station code %q", ErrInvalidInput, c.StationCode)
	case c.Count < 1 || c.Count > wire.MaxProbes:
		return fmt.Errorf("%w: link check count %d outside 1..%d", ErrInvalidInput, c.Count, wire.MaxProbes)
	case c.SpacingSec < 1:
		return fmt.Errorf("%w: link check spacing %d s", ErrInvalidInput, c.SpacingSec)
	}
	return nil
}

func normLinkCheck(c *LinkCheck) {
	c.RequestedAt = normTime(c.RequestedAt)
	c.StartedAt, c.FinishedAt = normTimePtr(c.StartedAt), normTimePtr(c.FinishedAt)
}

// CreateLinkCheck records a requested run. Only one run may be requested
// or running at a time (ErrConflict otherwise).
func (s *Store) CreateLinkCheck(ctx context.Context, c LinkCheck) (LinkCheck, error) {
	c.ID, c.State = 0, LinkCheckRequested
	if err := validLinkCheck(c); err != nil {
		return LinkCheck{}, err
	}
	normLinkCheck(&c)
	if err := s.db.WithContext(ctx).Create(&c).Error; err != nil {
		return LinkCheck{}, mapDBError(err)
	}
	return c, nil
}

// UpdateLinkCheck saves a run's progress or result.
func (s *Store) UpdateLinkCheck(ctx context.Context, c LinkCheck) error {
	if c.ID == 0 {
		return fmt.Errorf("%w: link check id", ErrInvalidInput)
	}
	normLinkCheck(&c)
	return rowsOrNotFound(s.db.WithContext(ctx).Save(&c))
}

func (s *Store) firstLinkCheck(ctx context.Context, where string, args ...any) (LinkCheck, error) {
	var c LinkCheck
	err := s.db.WithContext(ctx).Where(where, args...).Order("id").Take(&c).Error
	if err != nil {
		return LinkCheck{}, mapDBError(err)
	}
	readLinkCheck(&c)
	return c, nil
}

func readLinkCheck(c *LinkCheck) {
	c.RequestedAt = normTime(c.RequestedAt)
	c.StartedAt, c.FinishedAt = normTimePtr(c.StartedAt), normTimePtr(c.FinishedAt)
}

// GetLinkCheck returns one run.
func (s *Store) GetLinkCheck(ctx context.Context, id uint) (LinkCheck, error) {
	return s.firstLinkCheck(ctx, "id = ?", id)
}

// NextRequestedLinkCheck returns the run waiting to start (ErrNotFound if none).
func (s *Store) NextRequestedLinkCheck(ctx context.Context) (LinkCheck, error) {
	return s.firstLinkCheck(ctx, "state = ?", LinkCheckRequested)
}

// ActiveLinkCheck returns the running run (ErrNotFound if none).
func (s *Store) ActiveLinkCheck(ctx context.Context) (LinkCheck, error) {
	return s.firstLinkCheck(ctx, "state = ?", LinkCheckRunning)
}

// ListLinkChecks returns the newest runs first.
func (s *Store) ListLinkChecks(ctx context.Context, limit int) ([]LinkCheck, error) {
	var out []LinkCheck
	if err := s.db.WithContext(ctx).Order("id DESC").Limit(limit).Find(&out).Error; err != nil {
		return nil, mapDBError(err)
	}
	for i := range out {
		readLinkCheck(&out[i])
	}
	return out, nil
}

// SaveLinkProbe inserts or replaces one probe's record.
func (s *Store) SaveLinkProbe(ctx context.Context, p LinkProbe) error {
	if p.CheckID == 0 || p.Idx < 1 || p.Idx > wire.MaxProbes {
		return fmt.Errorf("%w: link probe %d/%d", ErrInvalidInput, p.CheckID, p.Idx)
	}
	p.SentAt, p.AckedAt = normTimePtr(p.SentAt), normTimePtr(p.AckedAt)
	return mapDBError(s.db.WithContext(ctx).Save(&p).Error)
}

// ListLinkProbes returns a run's probes in order.
func (s *Store) ListLinkProbes(ctx context.Context, checkID uint) ([]LinkProbe, error) {
	var out []LinkProbe
	err := s.db.WithContext(ctx).Where("check_id = ?", checkID).Order("idx").Find(&out).Error
	return out, mapDBError(err)
}

// LinkProbeByMessage finds the probe sent as graywolf row gwID.
func (s *Store) LinkProbeByMessage(ctx context.Context, gwID uint64) (LinkProbe, error) {
	var p LinkProbe
	err := s.db.WithContext(ctx).Where("gw_message_id = ?", gwID).Take(&p).Error
	return p, mapDBError(err)
}

// RecordProbeHeard adds a decoded probe to its run's response record,
// creating it on the first probe. A repeat changes nothing. The first
// level and path seen are kept until a frame brings a newer reading.
func (s *Store) RecordProbeHeard(ctx context.Context, h ProbeHeard) (LinkResponse, error) {
	if !ValidStationCall(h.PeerCall) || !wire.ValidCheckpointCode(h.ProberCode) ||
		h.Total < 1 || h.Total > wire.MaxProbes || h.Idx < 1 || h.Idx > h.Total || h.Run < 1 {
		return LinkResponse{}, fmt.Errorf("%w: probe %+v", ErrInvalidInput, h)
	}
	at := normTime(h.At)
	var out LinkResponse
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var r LinkResponse
		err := tx.Where("peer_call = ? AND run = ?", h.PeerCall, h.Run).Take(&r).Error
		if err == nil && at.Sub(normTime(r.LastHeardAt)) > runReuseGap {
			// The same run number from the same station much later is a
			// new run (numbers are random 1..9999), not a late probe.
			if err := tx.Delete(&LinkResponse{}, r.ID).Error; err != nil {
				return err
			}
			err = gorm.ErrRecordNotFound
		}
		switch {
		case err == gorm.ErrRecordNotFound:
			if err := tx.Where("last_heard_at < ?", at.Add(-responseKeep)).Delete(&LinkResponse{}).Error; err != nil {
				return err
			}
			r = LinkResponse{PeerCall: h.PeerCall, ProberCode: h.ProberCode, Run: h.Run, Total: h.Total,
				FirstHeardAt: at, LastHeardAt: at, UnknownPeer: h.UnknownPeer}
		case err != nil:
			return err
		}
		heard := r.HeardIndices()
		if slices.Contains(heard, uint8(h.Idx)) {
			out = r
			return nil
		}
		heard = append(heard, uint8(h.Idx))
		slices.Sort(heard)
		parts := make([]string, len(heard))
		for i, n := range heard {
			parts[i] = strconv.Itoa(int(n))
		}
		r.Heard = strings.Join(parts, ",")
		if at.After(r.LastHeardAt) {
			r.LastHeardAt = at
		}
		if h.Level != nil {
			r.Level = h.Level
		}
		if h.Via != "" {
			r.Via = h.Via
		}
		if err := tx.Save(&r).Error; err != nil {
			return err
		}
		out = r
		return nil
	})
	if err != nil {
		return LinkResponse{}, mapDBError(err)
	}
	readLinkResponse(&out)
	return out, nil
}

func readLinkResponse(r *LinkResponse) {
	r.FirstHeardAt, r.LastHeardAt = normTime(r.FirstHeardAt), normTime(r.LastHeardAt)
	r.ReplyDueAt, r.ReplySentAt, r.ReplyAckedAt = normTimePtr(r.ReplyDueAt), normTimePtr(r.ReplySentAt), normTimePtr(r.ReplyAckedAt)
}

// UpdateLinkResponse saves a response's reply bookkeeping.
func (s *Store) UpdateLinkResponse(ctx context.Context, r LinkResponse) error {
	if r.ID == 0 {
		return fmt.Errorf("%w: link response id", ErrInvalidInput)
	}
	r.ReplyDueAt, r.ReplySentAt, r.ReplyAckedAt = normTimePtr(r.ReplyDueAt), normTimePtr(r.ReplySentAt), normTimePtr(r.ReplyAckedAt)
	return rowsOrNotFound(s.db.WithContext(ctx).Save(&r))
}

func (s *Store) findLinkResponses(ctx context.Context, q *gorm.DB) ([]LinkResponse, error) {
	var out []LinkResponse
	if err := q.WithContext(ctx).Find(&out).Error; err != nil {
		return nil, mapDBError(err)
	}
	for i := range out {
		readLinkResponse(&out[i])
	}
	return out, nil
}

// DueLinkResponses returns responses whose reply is due by now.
func (s *Store) DueLinkResponses(ctx context.Context, now time.Time) ([]LinkResponse, error) {
	return s.findLinkResponses(ctx, s.db.Where("reply_due_at IS NOT NULL AND reply_due_at <= ?", normTime(now)).Order("reply_due_at"))
}

// LinkResponseByReply finds the response whose reply is graywolf row gwID.
func (s *Store) LinkResponseByReply(ctx context.Context, gwID uint64) (LinkResponse, error) {
	var r LinkResponse
	if err := s.db.WithContext(ctx).Where("reply_gw_id = ?", gwID).Take(&r).Error; err != nil {
		return LinkResponse{}, mapDBError(err)
	}
	readLinkResponse(&r)
	return r, nil
}

// CountLinkSendsSince counts reply transmissions (first sends and
// resends) of responses first answered at or after t: the responder's
// airtime budget.
func (s *Store) CountLinkSendsSince(ctx context.Context, t time.Time) (int, error) {
	var n *int64
	err := s.db.WithContext(ctx).Model(&LinkResponse{}).Select("SUM(reply_attempts)").
		Where("reply_sent_at >= ?", normTime(t)).Scan(&n).Error
	if n == nil {
		return 0, mapDBError(err)
	}
	return int(*n), mapDBError(err)
}

// ListLinkResponses returns the newest responses first.
func (s *Store) ListLinkResponses(ctx context.Context, limit int) ([]LinkResponse, error) {
	return s.findLinkResponses(ctx, s.db.Order("last_heard_at DESC, id DESC").Limit(limit))
}

// CancelLinkCheck stops a requested or running run, recording why. It
// reports false if the run had already finished.
func (s *Store) CancelLinkCheck(ctx context.Context, id uint, reason string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&LinkCheck{}).
		Where("id = ? AND state IN ?", id, []string{LinkCheckRequested, LinkCheckRunning}).
		Updates(map[string]any{"state": LinkCheckCancelled, "error": truncateUTF8(reason, maxBadReportReason), "finished_at": normTime(s.now())})
	return res.RowsAffected == 1, mapDBError(res.Error)
}

// PendingLinkCheck returns the running run, else the oldest requested
// one (ErrNotFound if neither): the prober's one query per tick.
func (s *Store) PendingLinkCheck(ctx context.Context) (LinkCheck, error) {
	return s.firstLinkCheck(ctx, "state IN ?", []string{LinkCheckRunning, LinkCheckRequested})
}

// Guarded transitions. Each writes only its own columns and only from
// the state it expects, so the service tick, the inbox reader, the web
// UI and the CLI (another process) can't overwrite each other's changes
// with a stale copy of the row.

// LinkResult is what finishing a run records.
type LinkResult struct {
	FinishedAt  time.Time
	Verdict     string
	RoundTrip   int
	MedianRTTms *int
	LocalLevel  *int
	Advice      string
}

func (s *Store) guardedLinkCheck(ctx context.Context, where string, args []any, set map[string]any) (bool, error) {
	res := s.db.WithContext(ctx).Model(&LinkCheck{}).Where(where, args...).Updates(set)
	return res.RowsAffected == 1, mapDBError(res.Error)
}

// StartLinkCheck moves a requested run to running (false if it was
// cancelled meanwhile).
func (s *Store) StartLinkCheck(ctx context.Context, id uint, run int, at time.Time) (bool, error) {
	return s.guardedLinkCheck(ctx, "id = ? AND state = ?", []any{id, LinkCheckRequested},
		map[string]any{"state": LinkCheckRunning, "run": run, "started_at": normTime(at)})
}

// RecordLinkReply stores the responder's summary on a running run, once.
func (s *Store) RecordLinkReply(ctx context.Context, id uint, uplink int, remoteLevel *int, via string) (bool, error) {
	return s.guardedLinkCheck(ctx, "id = ? AND state = ? AND reply_received = ?", []any{id, LinkCheckRunning, false},
		map[string]any{"reply_received": true, "uplink": uplink, "remote_level": remoteLevel, "via": via})
}

// FinishLinkCheck grades a running run. replySeen is the reply flag the
// result was computed with: if a reply arrived since, nothing is
// written and the caller recomputes on its next tick.
func (s *Store) FinishLinkCheck(ctx context.Context, id uint, replySeen bool, r LinkResult) (bool, error) {
	return s.guardedLinkCheck(ctx, "id = ? AND state = ? AND reply_received = ?", []any{id, LinkCheckRunning, replySeen},
		map[string]any{"state": LinkCheckDone, "finished_at": normTime(r.FinishedAt), "verdict": r.Verdict,
			"round_trip": r.RoundTrip, "median_rtt_ms": r.MedianRTTms, "local_level": r.LocalLevel, "advice": r.Advice})
}

// NoteLinkCheckError records a problem on a run without changing its state.
func (s *Store) NoteLinkCheckError(ctx context.Context, id uint, msg string) error {
	return mapDBError(s.db.WithContext(ctx).Model(&LinkCheck{}).Where("id = ?", id).
		Update("error", truncateUTF8(msg, maxBadReportReason)).Error)
}

// AckLinkProbe records the first ACK of probe row gwID.
func (s *Store) AckLinkProbe(ctx context.Context, gwID uint64, at time.Time, rttMs int) (bool, error) {
	res := s.db.WithContext(ctx).Model(&LinkProbe{}).Where("gw_message_id = ? AND acked_at IS NULL", gwID).
		Updates(map[string]any{"acked_at": normTime(at), "rtt_ms": rttMs})
	return res.RowsAffected > 0, mapDBError(res.Error)
}

func (s *Store) guardedLinkResponse(ctx context.Context, where string, args []any, set map[string]any) (bool, error) {
	res := s.db.WithContext(ctx).Model(&LinkResponse{}).Where(where, args...).Updates(set)
	return res.RowsAffected == 1, mapDBError(res.Error)
}

// ScheduleLinkReply sets when a not-yet-sent reply goes out.
func (s *Store) ScheduleLinkReply(ctx context.Context, id uint, due time.Time) (bool, error) {
	return s.guardedLinkResponse(ctx, "id = ? AND reply_sent_at IS NULL AND reply_attempts = 0", []any{id},
		map[string]any{"reply_due_at": normTime(due)})
}

// MarkLinkReplySent records a reply transmission (first or resend)
// unless the reply was ACKed meanwhile.
func (s *Store) MarkLinkReplySent(ctx context.Context, id uint, gwID uint64, at, next time.Time) (bool, error) {
	return s.guardedLinkResponse(ctx, "id = ? AND reply_acked_at IS NULL", []any{id}, map[string]any{
		"reply_gw_id": gwID, "reply_attempts": gorm.Expr("reply_attempts + 1"),
		"reply_sent_at": gorm.Expr("COALESCE(reply_sent_at, ?)", normTime(at)), "reply_due_at": normTime(next),
	})
}

// RescheduleLinkReply moves an unACKed reply's next try, optionally
// counting a failed try.
func (s *Store) RescheduleLinkReply(ctx context.Context, id uint, next time.Time, countTry bool) (bool, error) {
	set := map[string]any{"reply_due_at": normTime(next)}
	if countTry {
		set["reply_attempts"] = gorm.Expr("reply_attempts + 1")
	}
	return s.guardedLinkResponse(ctx, "id = ? AND reply_acked_at IS NULL", []any{id}, set)
}

// StopLinkReply gives up on (or finishes) a response's reply.
func (s *Store) StopLinkReply(ctx context.Context, id uint) error {
	return mapDBError(s.db.WithContext(ctx).Model(&LinkResponse{}).Where("id = ?", id).Update("reply_due_at", nil).Error)
}

// AckLinkReply records the ACK of reply row gwID.
func (s *Store) AckLinkReply(ctx context.Context, gwID uint64, at time.Time) (bool, error) {
	return s.guardedLinkResponse(ctx, "reply_gw_id = ? AND reply_acked_at IS NULL", []any{gwID},
		map[string]any{"reply_acked_at": normTime(at), "reply_due_at": nil})
}

// GetLinkResponse returns one response.
func (s *Store) GetLinkResponse(ctx context.Context, id uint) (LinkResponse, error) {
	var r LinkResponse
	if err := s.db.WithContext(ctx).Where("id = ?", id).Take(&r).Error; err != nil {
		return LinkResponse{}, mapDBError(err)
	}
	readLinkResponse(&r)
	return r, nil
}
