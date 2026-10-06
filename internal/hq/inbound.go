package hq

import (
	"context"
	"fmt"
	"strings"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// HandleInbound stores an RC1 DM addressed to HQ (inbox.Dispatcher).
// graywolf has already ACKed it, so HQ can't REJ: an undecodable body
// is kept as a bad report instead. Bad input returns nil (not worth a
// retry); only store failures return an error, so the inbox reader
// retries the row and nothing graywolf ACKed is lost.
func (e *Engine) HandleInbound(ctx context.Context, m graywolf.Message) error {
	sender := strings.ToUpper(strings.TrimSpace(m.FromCall))
	if !store.ValidStationCall(sender) {
		// Never let arbitrary bytes reach the store, the API, CSV
		// exports, logs, or a gap request's addressee.
		e.log.Debug("hq: dropping RC1 from an invalid source callsign", "len", len(m.FromCall))
		return nil
	}
	msg, err := wire.Decode(m.Text)
	if err != nil {
		e.log.Warn("hq: undecodable RC1", "from", sender, "err", err)
		return e.store.RecordBadReport(ctx, m.ID, sender, guessCode(m.Text), m.Text, err.Error(), e.now())
	}
	raceNow, synced := e.clock.Now()
	switch msg := msg.(type) {
	case *wire.Report:
		res, err := e.store.IngestReport(ctx, msg, sender, m.ID, raceNow)
		if err != nil {
			return fmt.Errorf("hq: store report %s/%d: %w", msg.CP, msg.Seq, err)
		}
		if res.SeqReused {
			e.log.Warn("hq: checkpoint reused a batch seq with new data (DB reset?)", "cp", msg.CP, "seq", msg.Seq, "from", sender)
		}
		e.log.Debug("hq: report", "cp", msg.CP, "seq", msg.Seq, "from", sender, "entries", res.Entries, "duplicate", res.Duplicate)
	case *wire.Heartbeat:
		if err := e.store.RecordHeartbeat(ctx, msg, sender, raceNow, raceNow, synced); err != nil {
			return fmt.Errorf("hq: store heartbeat %s: %w", msg.CP, err)
		}
	default:
		// Gap requests are HQ→checkpoint; probes belong to the link
		// check (phase 12).
		e.log.Debug("hq: ignoring RC1 message type at HQ", "from", sender, "text", m.Text)
	}
	return nil
}

// HandleOutbound sees HQ's own sent RC1 rows (gap requests). Their
// delivery status doesn't matter: HQ repeats a request until the batch
// arrives (inbox.Dispatcher).
func (e *Engine) HandleOutbound(context.Context, graywolf.Message) error { return nil }

// guessCode returns the checkpoint-code field of RC1 text that failed to
// decode ("RC1 <type> <cp> ..."), or "" if there isn't a plausible one.
func guessCode(text string) string {
	fields := strings.Fields(strings.TrimPrefix(text, wire.Prefix))
	if len(fields) < 2 || !wire.ValidCheckpointCode(fields[1]) {
		return ""
	}
	return fields[1]
}
