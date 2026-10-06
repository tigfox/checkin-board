// Package app assembles checkin-board: store, graywolf client, race
// clock, both engines and the inbox reader, and runs them together.
package app

import (
	"context"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/inbox"
	"checkin-board/internal/store"
	"checkin-board/internal/wire"
)

// handler is one engine's inbox side.
type handler interface {
	HandleInbound(ctx context.Context, m graywolf.Message) error
	HandleOutbound(ctx context.Context, m graywolf.Message) error
}

// linkHandler is the link check's inbox side: it gets RC1 P and Q
// traffic, already decoded.
type linkHandler interface {
	HandleInbound(ctx context.Context, m graywolf.Message, msg wire.Message) error
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
//
// Link-check probes and replies (RC1 P, Q) go to the link check, never
// to the engines.
type Dispatcher struct {
	settings   settingsSource
	checkpoint handler
	hq         handler
	link       linkHandler // nil: link-check traffic is ignored
}

// NewDispatcher returns a Dispatcher over the two engines and the link
// check (which may be nil).
func NewDispatcher(settings settingsSource, checkpoint, hq handler, link linkHandler) *Dispatcher {
	return &Dispatcher{settings: settings, checkpoint: checkpoint, hq: hq, link: link}
}

// linkMessage decodes m if it is link-check traffic.
func linkMessage(m graywolf.Message) (wire.Message, bool) {
	msg, err := wire.Decode(m.Text)
	if err != nil {
		return nil, false
	}
	switch msg.(type) {
	case *wire.Probe, *wire.ProbeReply:
		return msg, true
	}
	return nil, false
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
	if msg, ok := linkMessage(m); ok {
		if d.link == nil {
			return nil
		}
		return d.link.HandleInbound(ctx, m, msg)
	}
	return h.HandleInbound(ctx, m)
}

// HandleOutbound implements inbox.Dispatcher.
func (d *Dispatcher) HandleOutbound(ctx context.Context, m graywolf.Message) error {
	h, err := d.target(ctx)
	if err != nil {
		return err
	}
	if _, ok := linkMessage(m); ok {
		if d.link == nil {
			return nil
		}
		return d.link.HandleOutbound(ctx, m)
	}
	return h.HandleOutbound(ctx, m)
}
