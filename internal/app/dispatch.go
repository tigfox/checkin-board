// Package app assembles checkin-board: store, graywolf client, race
// clock, both engines and the inbox reader, and runs them together.
package app

import (
	"context"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/inbox"
	"checkin-board/internal/store"
)

// handler is one engine's inbox side.
type handler interface {
	HandleInbound(ctx context.Context, m graywolf.Message) error
	HandleOutbound(ctx context.Context, m graywolf.Message) error
}

// settingsSource reads the node's current settings.
type settingsSource interface {
	GetSettings(ctx context.Context) (store.Settings, error)
}

// Dispatcher routes inbox traffic to the engine for the node's current
// role, read per row so a role change takes effect at once. A node with
// no role yet answers inbox.ErrNotReady: the reader holds its position
// without dropping anything, so reports graywolf ACKed before HQ was
// configured are ingested once it is.
type Dispatcher struct {
	settings   settingsSource
	checkpoint handler
	hq         handler
}

// NewDispatcher returns a Dispatcher over the two engines.
func NewDispatcher(settings settingsSource, checkpoint, hq handler) *Dispatcher {
	return &Dispatcher{settings: settings, checkpoint: checkpoint, hq: hq}
}

func (d *Dispatcher) target(ctx context.Context) (handler, error) {
	cfg, err := d.settings.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	switch cfg.Role {
	case store.RoleCheckpoint:
		return d.checkpoint, nil
	case store.RoleHQ:
		return d.hq, nil
	default:
		return nil, inbox.ErrNotReady
	}
}

// HandleInbound implements inbox.Dispatcher.
func (d *Dispatcher) HandleInbound(ctx context.Context, m graywolf.Message) error {
	h, err := d.target(ctx)
	if err != nil {
		return err
	}
	return h.HandleInbound(ctx, m)
}

// HandleOutbound implements inbox.Dispatcher.
func (d *Dispatcher) HandleOutbound(ctx context.Context, m graywolf.Message) error {
	h, err := d.target(ctx)
	if err != nil {
		return err
	}
	return h.HandleOutbound(ctx, m)
}
