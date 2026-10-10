package graywolf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
)

// graywolf settings a race config file can carry (phase 12a). Each is
// read, changed and written back whole: graywolf's PUTs replace the
// record.

// TxTiming is one channel's transmit timing (GET /api/tx-timing).
type TxTiming struct {
	ID        uint32 `json:"id,omitempty"`
	Channel   uint32 `json:"channel"`
	TxDelayMS int    `json:"tx_delay_ms"`
	TxTailMS  int    `json:"tx_tail_ms"`
	SlotMS    int    `json:"slot_ms"`
	Persist   int    `json:"persist"`
	FullDup   bool   `json:"full_dup"`
	Rate1Min  int    `json:"rate_1min"`
	Rate5Min  int    `json:"rate_5min"`
}

// Digipeater is graywolf's digipeater switch (GET /api/digipeater).
type Digipeater struct {
	ID                  uint32 `json:"id,omitempty"`
	Enabled             bool   `json:"enabled"`
	DedupeWindowSeconds int    `json:"dedupe_window_seconds"`
	MyCall              string `json:"my_call"`
}

// TxTimings calls GET /api/tx-timing.
func (c *Client) TxTimings(ctx context.Context) ([]TxTiming, error) {
	var out []TxTiming
	err := c.do(ctx, http.MethodGet, "/api/tx-timing", nil, nil, &out)
	return out, err
}

// SetTxTiming replaces one channel's timing via PUT /api/tx-timing/{id}.
func (c *Client) SetTxTiming(ctx context.Context, t TxTiming) (TxTiming, error) {
	if t.ID == 0 {
		return TxTiming{}, errors.New("graywolf: tx timing has no id")
	}
	body := t
	body.ID = 0 // the id is in the path
	var out TxTiming
	err := c.do(ctx, http.MethodPut, fmt.Sprintf("/api/tx-timing/%d", t.ID), nil, body, &out)
	return out, err
}

// Digipeater calls GET /api/digipeater.
func (c *Client) Digipeater(ctx context.Context) (Digipeater, error) {
	var out Digipeater
	err := c.do(ctx, http.MethodGet, "/api/digipeater", nil, nil, &out)
	return out, err
}

// SetDigipeater replaces the digipeater settings via PUT /api/digipeater.
func (c *Client) SetDigipeater(ctx context.Context, d Digipeater) (Digipeater, error) {
	body := d
	body.ID = 0
	var out Digipeater
	err := c.do(ctx, http.MethodPut, "/api/digipeater", nil, body, &out)
	return out, err
}

// SetMessagePreferences replaces graywolf's message preferences via
// PUT /api/messages/preferences.
func (c *Client) SetMessagePreferences(ctx context.Context, p MessagePreferences) (MessagePreferences, error) {
	var out MessagePreferences
	err := c.do(ctx, http.MethodPut, "/api/messages/preferences", nil, p, &out)
	return out, err
}
