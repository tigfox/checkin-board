// Package raceconfig reads a race config file (phase 12a; proposal
// docs/specs/2026-10-10-race-config-proposal.md): one JSON file per race
// listing every station, from which a node takes its own settings, HQ's
// checkpoint list, an event page and some graywolf settings. Loading only
// sets values: everything stays editable on the device afterwards.
package raceconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"checkin-board/internal/store"
)

const (
	// Format is the file's format marker.
	Format = "checkin-board-race/1"
	// MaxFileBytes caps a file, so a bad one can't fill the SD card.
	MaxFileBytes = 256 << 10
	// MaxEventPageBytes caps the event page's text.
	MaxEventPageBytes = 64 << 10
	// HQ is the station id of the HQ entry.
	HQ = "hq"

	maxTxDelayMS     = 2000
	maxTxTailMS      = 1000
	maxRetentionDays = 3650
)

// ErrInvalid wraps every problem with a file.
var ErrInvalid = errors.New("race config")

// File is a parsed, validated race config. Optional fields (pointers,
// empty strings) leave the node's current value alone.
type File struct {
	Format      string       `json:"format"`
	Race        Race         `json:"race"`
	Messaging   *Messaging   `json:"messaging,omitempty"`
	HQ          HQStation    `json:"hq"`
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
	EventPage   string       `json:"event_page,omitempty"`
	Graywolf    *Graywolf    `json:"graywolf,omitempty"`

	digest string // sha256 of the file's bytes, for the preview token
}

// Race is the race-wide part.
type Race struct {
	Name string `json:"name,omitempty"`
}

// Messaging is the Messaging settings (store.Settings tunables).
type Messaging struct {
	Path          *string `json:"path,omitempty"`
	GWChannel     *int    `json:"gw_channel,omitempty"`
	HeartbeatSec  *int    `json:"heartbeat_sec,omitempty"`
	FlushAfterSec *int    `json:"flush_after_sec,omitempty"`
	MaxInFlight   *int    `json:"max_in_flight,omitempty"`
	MaxTextLen    *int    `json:"max_text_len,omitempty"`
	GapGraceSec   *int    `json:"gap_grace_sec,omitempty"`
}

// HQStation is HQ's entry.
type HQStation struct {
	Callsign    string   `json:"callsign,omitempty"`
	StationName string   `json:"station_name,omitempty"`
	LocalCodes  []string `json:"local_codes,omitempty"`
}

// Checkpoint is one checkpoint's entry: HQ's list row and that node's
// own settings.
type Checkpoint struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	StationName string `json:"station_name,omitempty"`
	Callsign    string `json:"callsign,omitempty"`
	CourseOrder int    `json:"course_order,omitempty"`
}

// Graywolf is the graywolf part, applied through graywolf's API with its
// own confirm. Node hardware (audio device, PTT, sample rate) never is.
type Graywolf struct {
	TxDelayMS     *int  `json:"tx_delay_ms,omitempty"`
	TxTailMS      *int  `json:"tx_tail_ms,omitempty"`
	RetentionDays *int  `json:"message_retention_days,omitempty"`
	Digipeater    *bool `json:"digipeater,omitempty"`
}

// Station is one entry a node can be.
type Station struct {
	ID       string `json:"id"` // HQ or a checkpoint code
	Role     string `json:"role"`
	Label    string `json:"label"`
	Callsign string `json:"callsign,omitempty"`
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// secretRe: field names that could hold a secret. A race config is meant
// to be shared, so none is accepted, with a clearer error than "unknown".
var secretRe = regexp.MustCompile(`(?i)unknown field "[^"]*(password|passwd|token|secret|key)[^"]*"`)

// Parse reads and validates a race config file.
func Parse(r io.Reader) (*File, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return nil, invalid("read: %v", err)
	}
	if len(b) > MaxFileBytes {
		return nil, invalid("the file is larger than %d KB", MaxFileBytes>>10)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var f File
	if err := dec.Decode(&f); err != nil {
		if secretRe.MatchString(err.Error()) {
			return nil, invalid("secrets (passwords, tokens) don't belong in a race config file: %v", err)
		}
		if strings.Contains(err.Error(), "unknown field") {
			return nil, invalid("%v", err)
		}
		return nil, invalid("not valid JSON: %v", err)
	}
	if dec.More() {
		return nil, invalid("extra data after the JSON object")
	}
	if err := f.validate(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	f.digest = hex.EncodeToString(sum[:])
	return &f, nil
}

// HasStation reports whether id is one of the file's stations.
func (f *File) HasStation(id string) bool {
	for _, st := range f.Stations() {
		if st.ID == id {
			return true
		}
	}
	return false
}

func (f *File) validate() error {
	if f.Format != Format {
		return invalid("format %q, want %q", f.Format, Format)
	}
	if !utf8.ValidString(f.EventPage) || len(f.EventPage) > MaxEventPageBytes {
		return invalid("event_page must be UTF-8 text of at most %d KB", MaxEventPageBytes>>10)
	}
	f.normalize()
	if f.HQ.Callsign != "" && !store.ValidStationCall(f.HQ.Callsign) {
		return invalid("hq.callsign %q must be a station callsign (optional -SSID 0-15)", f.HQ.Callsign)
	}
	if len(f.Checkpoints) > 0 && f.HQ.Callsign == "" {
		return invalid("hq.callsign is needed: the checkpoints send their reports to it")
	}
	if err := f.validateUnique(); err != nil {
		return err
	}
	for i, c := range f.Checkpoints {
		row := store.Checkpoint{Code: c.Code, Name: c.Name, CourseOrder: c.CourseOrder, ExpectedCall: c.Callsign}
		if err := row.Validate(); err != nil {
			return invalid("checkpoints[%d]: %v", i, err)
		}
	}
	if err := f.validateGraywolf(); err != nil {
		return err
	}
	// The messaging tunables on their own (a file may list no stations),
	// then every station's resulting settings, by the app's own rules.
	if f.Messaging != nil {
		s := store.DefaultSettings()
		f.applyMessaging(&s)
		if err := s.Validate(); err != nil {
			return invalid("messaging: %v", err)
		}
	}
	for _, st := range f.Stations() {
		if _, err := f.SettingsFor(st.ID, f.validationBase()); err != nil {
			return err
		}
	}
	return nil
}

// validationBase stands in for a node's own settings when checking a
// file: what the file leaves out, the node supplies (HQ's local codes
// here, a code none of the file's checkpoints uses; the real check is
// the preview, on the node's own settings).
func (f *File) validationBase() store.Settings {
	s := store.DefaultSettings()
	for i := 0; ; i++ {
		code := "LOCAL" + strconv.Itoa(i)
		if f.checkpoint(code) == nil {
			s.HQLocalCodes = code
			return s
		}
	}
}

// normalize upper-cases codes and callsigns, and trims names, as the
// settings forms do.
func (f *File) normalize() {
	up := func(s string) string { return strings.ToUpper(strings.TrimSpace(s)) }
	f.Race.Name = strings.TrimSpace(f.Race.Name)
	f.HQ.Callsign = up(f.HQ.Callsign)
	for i := range f.HQ.LocalCodes {
		f.HQ.LocalCodes[i] = up(f.HQ.LocalCodes[i])
	}
	for i := range f.Checkpoints {
		c := &f.Checkpoints[i]
		c.Code, c.Callsign, c.Name = up(c.Code), up(c.Callsign), strings.TrimSpace(c.Name)
	}
	if m := f.Messaging; m != nil && m.Path != nil {
		p := strings.ToUpper(strings.ReplaceAll(*m.Path, " ", ""))
		m.Path = &p
	}
}

func (f *File) validateUnique() error {
	codes := map[string]string{}
	calls := map[string]string{}
	addCall := func(call, who string) error {
		if call == "" {
			return nil
		}
		key := strings.TrimSuffix(call, "-0") // graywolf: -0 is no SSID
		if prev, ok := calls[key]; ok {
			return invalid("callsign %s is listed twice (%s and %s): every station needs its own callsign-SSID", call, prev, who)
		}
		calls[key] = who
		return nil
	}
	if err := addCall(f.HQ.Callsign, "hq"); err != nil {
		return err
	}
	for i, c := range f.Checkpoints {
		who := fmt.Sprintf("checkpoints[%d]", i)
		if prev, ok := codes[c.Code]; ok {
			return invalid("checkpoint code %s is listed twice (%s and %s)", c.Code, prev, who)
		}
		codes[c.Code] = who
		if err := addCall(c.Callsign, who); err != nil {
			return err
		}
	}
	for _, lc := range f.HQ.LocalCodes {
		if who, ok := codes[lc]; ok {
			return invalid("HQ local code %s is also a checkpoint code (%s)", lc, who)
		}
	}
	return nil
}

func (f *File) validateGraywolf() error {
	g := f.Graywolf
	if g == nil {
		return nil
	}
	check := func(name string, v *int, hi int) error {
		if v != nil && (*v < 0 || *v > hi) {
			return invalid("graywolf.%s %d outside 0..%d", name, *v, hi)
		}
		return nil
	}
	return errors.Join(check("tx_delay_ms", g.TxDelayMS, maxTxDelayMS), check("tx_tail_ms", g.TxTailMS, maxTxTailMS),
		check("message_retention_days", g.RetentionDays, maxRetentionDays))
}

// Stations lists the entries a node can be: HQ (if the file has one),
// then the checkpoints in file order.
func (f *File) Stations() []Station {
	var out []Station
	if f.HQ.Callsign != "" || len(f.HQ.LocalCodes) > 0 {
		label := "HQ"
		if f.HQ.Callsign != "" {
			label += " (" + f.HQ.Callsign + ")"
		}
		out = append(out, Station{ID: HQ, Role: store.RoleHQ, Label: label, Callsign: f.HQ.Callsign})
	}
	for _, c := range f.Checkpoints {
		label := strings.TrimSpace(c.Code + " " + c.Name)
		if c.Callsign != "" {
			label += " (" + c.Callsign + ")"
		}
		out = append(out, Station{ID: c.Code, Role: store.RoleCheckpoint, Label: label, Callsign: c.Callsign})
	}
	return out
}

// StationFor is the station whose callsign is call (graywolf's own), to
// pre-select it; "" when none matches.
func (f *File) StationFor(call string) string {
	for _, st := range f.Stations() {
		if store.SameStation(st.Callsign, call) {
			return st.ID
		}
	}
	return ""
}

// StationCallsign is station id's callsign in the file ("" if none).
func (f *File) StationCallsign(id string) string {
	for _, st := range f.Stations() {
		if st.ID == id {
			return st.Callsign
		}
	}
	return ""
}

// SettingsFor returns base with the file applied for station id: role,
// race name, station name, codes, HQ callsign and messaging tunables.
// Fields the file leaves out keep base's values. The result passes the
// app's settings rules, or an error says why.
func (f *File) SettingsFor(id string, base store.Settings) (store.Settings, error) {
	s := base
	if f.Race.Name != "" {
		s.RaceName = f.Race.Name
	}
	switch {
	case id == HQ:
		s.Role = store.RoleHQ
		if f.HQ.StationName != "" {
			s.StationTactical = f.HQ.StationName
		}
		if len(f.HQ.LocalCodes) > 0 {
			s.HQLocalCodes = strings.Join(f.HQ.LocalCodes, ",")
		} else if s.HQLocalCodes == "" {
			return store.Settings{}, invalid("the file gives HQ no local codes and this node has none: add them on the Station page (Local codes), or to the file's hq.local_codes")
		} else {
			// The node's own local codes stay: they mustn't be a
			// checkpoint's code too.
			for _, lc := range strings.Split(s.HQLocalCodes, ",") {
				if f.checkpoint(lc) != nil {
					return store.Settings{}, invalid("this node's HQ local code %s is also a checkpoint code in the file: change it on the Station page first", lc)
				}
			}
		}
	default:
		c := f.checkpoint(id)
		if c == nil {
			return store.Settings{}, invalid("no station %q in the file", id)
		}
		s.Role, s.CheckpointCode = store.RoleCheckpoint, c.Code
		if c.StationName != "" {
			s.StationTactical = c.StationName
		}
		if f.HQ.Callsign != "" {
			s.HQCall = f.HQ.Callsign
		}
	}
	f.applyMessaging(&s)
	if err := s.Validate(); err != nil {
		where := "hq"
		for i, c := range f.Checkpoints {
			if c.Code == id {
				where = fmt.Sprintf("checkpoints[%d] (%s)", i, id)
			}
		}
		return store.Settings{}, invalid("%s: %v", where, err)
	}
	return s, nil
}

func (f *File) applyMessaging(s *store.Settings) {
	m := f.Messaging
	if m == nil {
		return
	}
	setStr(&s.Path, m.Path)
	setInt(&s.GWChannel, m.GWChannel)
	setInt(&s.HeartbeatSec, m.HeartbeatSec)
	setInt(&s.FlushAfterSec, m.FlushAfterSec)
	setInt(&s.MaxInFlight, m.MaxInFlight)
	setInt(&s.MaxTextLen, m.MaxTextLen)
	setInt(&s.GapGraceSec, m.GapGraceSec)
}

// CheckpointList is HQ's checkpoint list from the file.
func (f *File) CheckpointList() []store.Checkpoint {
	out := make([]store.Checkpoint, len(f.Checkpoints))
	for i, c := range f.Checkpoints {
		out[i] = store.Checkpoint{Code: c.Code, Name: c.Name, CourseOrder: c.CourseOrder, ExpectedCall: c.Callsign}
	}
	return out
}

func (f *File) checkpoint(code string) *Checkpoint {
	for i := range f.Checkpoints {
		if f.Checkpoints[i].Code == code {
			return &f.Checkpoints[i]
		}
	}
	return nil
}

func setStr(dst *string, v *string) {
	if v != nil {
		*dst = *v
	}
}

func setInt(dst *int, v *int) {
	if v != nil {
		*dst = *v
	}
}
