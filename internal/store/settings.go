package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"checkin-board/internal/wire"
)

// Role selects what a node does in the race network.
type Role = string

const (
	RoleUnset      Role = ""           // fresh node, not yet configured
	RoleCheckpoint Role = "checkpoint" // logs bibs and reports them to HQ
	RoleHQ         Role = "hq"         // collects reports; may also log locally
)

// Race lifecycle states (spec 4.7).
const (
	RaceSetup    = "setup"
	RaceActive   = "active"
	RaceComplete = "complete"
)

// Tunable bounds. Wide enough for field experimentation, tight enough
// that a typo can't flood the channel or stall delivery.
const (
	MaxRaceNameLen    = 64
	MaxTacticalLen    = 9
	MaxPathElements   = 8
	minTextLen        = wire.DefaultMaxTextLen
	maxTextLen        = 200 // graywolf's long-message ceiling
	minFlushSec       = 5
	maxFlushSec       = 300
	minInFlight       = 1
	maxInFlight       = 8
	minHeartbeatSec   = 60
	maxHeartbeatSec   = 3600
	minGapGraceSec    = 30
	maxGapGraceSec    = 900
	maxGraywolfChanID = 1 << 20
)

var (
	// stationCallRe: a station callsign with optional AX.25 SSID 0-15.
	// HQ must be a real station (DMs to it are ACKed), never a tactical
	// label.
	stationCallRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-([0-9]|1[0-5]))?$`)
	// tacticalRe: the station's voice-net name, e.g. AID3, FINISH.
	tacticalRe = regexp.MustCompile(`^[A-Z0-9][A-Z0-9-]{0,8}$`)
	// pathElemRe: one digipeater path element (callsign/alias, SSID 0-15).
	pathElemRe = regexp.MustCompile(`^[A-Z0-9]{1,6}(-([0-9]|1[0-5]))?$`)
)

// Settings is the singleton settings row (id = 1).
type Settings struct {
	ID            uint       `gorm:"column:id;primaryKey"`
	Role          Role       `gorm:"column:role"`
	RaceName      string     `gorm:"column:race_name"`
	RaceState     string     `gorm:"column:race_state"`
	RaceStartedAt *time.Time `gorm:"column:race_started_at"`
	// StationTactical is the station's voice-net name (spec 8.1).
	StationTactical string `gorm:"column:station_tactical"`
	CheckpointCode  string `gorm:"column:checkpoint_code"`
	// HQLocalCodes lists the codes HQ logs on its own keypad, comma
	// separated (e.g. "START,FIN").
	HQLocalCodes string `gorm:"column:hq_local_codes"`
	// HQCall is HQ's station callsign; checkpoints DM reports to it.
	HQCall string `gorm:"column:hq_call"`
	// GWChannel 0 means graywolf's default TX channel.
	GWChannel     int       `gorm:"column:gw_channel"`
	Path          string    `gorm:"column:path"`
	MaxTextLen    int       `gorm:"column:max_text_len"`
	FlushAfterSec int       `gorm:"column:flush_after_sec"`
	MaxInFlight   int       `gorm:"column:max_in_flight"`
	HeartbeatSec  int       `gorm:"column:heartbeat_sec"`
	GapGraceSec   int       `gorm:"column:gap_grace_sec"`
	UpdatedAt     time.Time `gorm:"column:updated_at;autoUpdateTime:false"`
}

func (Settings) TableName() string { return "settings" }

// DefaultSettings is the configuration of a node that has never been set up.
func DefaultSettings() Settings {
	return Settings{
		ID:            1,
		RaceState:     RaceSetup,
		MaxTextLen:    wire.DefaultMaxTextLen,
		FlushAfterSec: 20,
		// Window of 4: in pkg/race's latency simulation it drained a
		// 300-bib surge at 30% loss several times faster than 2 at
		// similar total airtime (spec 9b). Recheck on real RF.
		MaxInFlight:  4,
		HeartbeatSec: 300,
		GapGraceSec:  90,
	}
}

// LocalCodes returns the checkpoint codes this node logs bibs under:
// its own code on a checkpoint, the configured list on HQ, none when unset.
func (c Settings) LocalCodes() []string {
	switch c.Role {
	case RoleCheckpoint:
		return []string{c.CheckpointCode}
	case RoleHQ:
		if c.HQLocalCodes == "" {
			return nil
		}
		return strings.Split(c.HQLocalCodes, ",")
	default:
		return nil
	}
}

// Validate checks every field and returns an error wrapping
// ErrInvalidSettings that names the first bad field.
func (c Settings) Validate() error {
	if err := validateRole(c); err != nil {
		return err
	}
	switch c.RaceState {
	case RaceSetup, RaceActive, RaceComplete:
	default:
		return settingsErr("race_state %q must be setup, active, or complete", c.RaceState)
	}
	if c.RaceState != RaceSetup && c.Role == RoleUnset {
		return settingsErr("race_state %q needs a role", c.RaceState)
	}
	if len(c.RaceName) > MaxRaceNameLen || (c.RaceName != "" && !validName(c.RaceName, MaxRaceNameLen)) {
		return settingsErr("race_name must be at most %d printable characters", MaxRaceNameLen)
	}
	if c.StationTactical != "" && !tacticalRe.MatchString(c.StationTactical) {
		return settingsErr("station_tactical %q must be 1-%d of A-Z, 0-9, '-' starting with a letter or digit", c.StationTactical, MaxTacticalLen)
	}
	if c.GWChannel < 0 || c.GWChannel > maxGraywolfChanID {
		return settingsErr("gw_channel %d out of range", c.GWChannel)
	}
	if err := ValidatePath(c.Path); err != nil {
		return settingsErr("%v", err)
	}
	return validateTuning(c)
}

func validateRole(c Settings) error {
	switch c.Role {
	case RoleUnset:
		return nil
	case RoleCheckpoint:
		if !wire.ValidCheckpointCode(c.CheckpointCode) {
			return settingsErr("checkpoint_code %q must be 1-%d of A-Z, 0-9", c.CheckpointCode, wire.MaxCheckpointCodeLen)
		}
		if !ValidStationCall(c.HQCall) {
			return settingsErr("hq_call %q must be a station callsign (optional -SSID)", c.HQCall)
		}
	case RoleHQ:
		if err := validateCodeList(c.HQLocalCodes); err != nil {
			return err
		}
	default:
		return settingsErr("role %q must be checkpoint or hq", c.Role)
	}
	return nil
}

func validateCodeList(list string) error {
	if list == "" {
		return settingsErr("hq_local_codes must list at least one code")
	}
	seen := map[string]bool{}
	for _, code := range strings.Split(list, ",") {
		if !wire.ValidCheckpointCode(code) {
			return settingsErr("hq_local_codes entry %q must be 1-%d of A-Z, 0-9", code, wire.MaxCheckpointCodeLen)
		}
		if seen[code] {
			return settingsErr("hq_local_codes lists %q twice", code)
		}
		seen[code] = true
	}
	return nil
}

func validateTuning(c Settings) error {
	checks := []struct {
		name      string
		v, lo, hi int
	}{
		{"max_text_len", c.MaxTextLen, minTextLen, maxTextLen},
		{"flush_after_sec", c.FlushAfterSec, minFlushSec, maxFlushSec},
		{"max_in_flight", c.MaxInFlight, minInFlight, maxInFlight},
		{"heartbeat_sec", c.HeartbeatSec, minHeartbeatSec, maxHeartbeatSec},
		{"gap_grace_sec", c.GapGraceSec, minGapGraceSec, maxGapGraceSec},
	}
	for _, ck := range checks {
		if ck.v < ck.lo || ck.v > ck.hi {
			return settingsErr("%s %d outside %d..%d", ck.name, ck.v, ck.lo, ck.hi)
		}
	}
	return nil
}

// ValidStationCall reports whether s is an uppercase station callsign
// with an optional SSID 0-15.
func ValidStationCall(s string) bool { return stationCallRe.MatchString(s) }

// ValidatePath checks an APRS digipeater path such as "WIDE1-1,WIDE2-1":
// up to MaxPathElements comma-separated callsigns or aliases, uppercase,
// SSID 0-15. Empty means direct.
func ValidatePath(path string) error {
	if path == "" {
		return nil
	}
	elems := strings.Split(path, ",")
	if len(elems) > MaxPathElements {
		return fmt.Errorf("path %q has more than %d elements", path, MaxPathElements)
	}
	for _, el := range elems {
		if !pathElemRe.MatchString(el) {
			return fmt.Errorf("path element %q must be a callsign or alias with optional -SSID 0-15", el)
		}
	}
	return nil
}

func settingsErr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidSettings}, args...)...)
}

// GetSettings returns the settings row, or DefaultSettings when the node
// has never been configured. It never creates a row.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	var c Settings
	err := s.db.WithContext(ctx).First(&c, 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return DefaultSettings(), nil
	}
	if err != nil {
		return Settings{}, err
	}
	c.UpdatedAt = normTime(c.UpdatedAt)
	c.RaceStartedAt = normTimePtr(c.RaceStartedAt)
	return c, nil
}

// SaveSettings validates and writes the singleton row, returning what
// was stored.
func (s *Store) SaveSettings(ctx context.Context, c Settings) (Settings, error) {
	if err := c.Validate(); err != nil {
		return Settings{}, err
	}
	row := c
	row.ID = 1
	row.UpdatedAt = normTime(s.now())
	row.RaceStartedAt = normTimePtr(c.RaceStartedAt)
	if err := s.db.WithContext(ctx).Save(&row).Error; err != nil {
		return Settings{}, err
	}
	return row, nil
}
