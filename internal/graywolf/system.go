package graywolf

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// stationCallRe is a station callsign with an optional 1-2 char SSID.
var stationCallRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-[A-Z0-9]{1,2})?$`)

// Health calls GET /api/health.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var out Health
	err := c.do(ctx, http.MethodGet, "/api/health", nil, nil, &out)
	return out, err
}

// Version calls GET /api/version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	err := c.do(ctx, http.MethodGet, "/api/version", nil, nil, &out)
	return out, err
}

// StationConfig calls GET /api/station/config.
func (c *Client) StationConfig(ctx context.Context) (StationConfig, error) {
	var out StationConfig
	err := c.do(ctx, http.MethodGet, "/api/station/config", nil, nil, &out)
	return out, err
}

// SetStationCallsign changes graywolf's station callsign via
// PUT /api/station/config. This affects all of graywolf, not just the
// race; callers must confirm with the operator first.
func (c *Client) SetStationCallsign(ctx context.Context, callsign string) (StationConfig, error) {
	call := strings.ToUpper(strings.TrimSpace(callsign))
	if !stationCallRe.MatchString(call) {
		return StationConfig{}, fmt.Errorf("graywolf: %q is not a valid station callsign", callsign)
	}
	var out StationConfig
	err := c.do(ctx, http.MethodPut, "/api/station/config", nil, StationConfig{Callsign: call}, &out)
	return out, err
}
