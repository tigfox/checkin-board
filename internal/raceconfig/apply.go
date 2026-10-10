package raceconfig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"checkin-board/internal/graywolf"
	"checkin-board/internal/store"
)

var (
	// ErrNotSetup: a race config applies only before the race starts.
	ErrNotSetup = errors.New("race config: applies only before the race starts (or after a reset)")
	// ErrNoBackup: there are no saved graywolf settings to restore.
	ErrNoBackup = errors.New("race config: no saved graywolf settings to restore")
	// ErrStale: the file, this node or graywolf changed since the preview
	// the operator saw; preview again.
	ErrStale = errors.New("race config: something changed since the preview; preview again before loading")
)

// GraywolfAPI is the graywolf API applying a config needs.
type GraywolfAPI interface {
	StationConfig(ctx context.Context) (graywolf.StationConfig, error)
	SetStationCallsign(ctx context.Context, callsign string) (graywolf.StationConfig, error)
	Channels(ctx context.Context) ([]graywolf.Channel, error)
	TxTimings(ctx context.Context) ([]graywolf.TxTiming, error)
	SetTxTiming(ctx context.Context, t graywolf.TxTiming) (graywolf.TxTiming, error)
	Digipeater(ctx context.Context) (graywolf.Digipeater, error)
	SetDigipeater(ctx context.Context, d graywolf.Digipeater) (graywolf.Digipeater, error)
	MessagePreferences(ctx context.Context) (graywolf.MessagePreferences, error)
	SetMessagePreferences(ctx context.Context, p graywolf.MessagePreferences) (graywolf.MessagePreferences, error)
}

// Service previews and applies race configs on this node. Apply and
// RestoreGraywolf are serialized.
type Service struct {
	st *store.Store
	gw GraywolfAPI
	mu sync.Mutex
}

// NewService returns a service.
func NewService(st *store.Store, gw GraywolfAPI) *Service { return &Service{st: st, gw: gw} }

// Change is one value a config would change.
type Change struct {
	Field string `json:"field"`
	Label string `json:"label"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// Plan is what applying a config as a station would change.
type Plan struct {
	Station     string   `json:"station"`
	Role        string   `json:"role"`
	Settings    []Change `json:"settings"`
	Checkpoints *Change  `json:"checkpoints,omitempty"` // HQ's list, summarized
	EventPage   *Change  `json:"event_page,omitempty"`
	Graywolf    []Change `json:"graywolf"` // applied only with their own opt-in
	CanApply    bool     `json:"can_apply"`
	Problems    []string `json:"problems,omitempty"`
	// Token ties an Apply to this preview: the file, the station and every
	// change shown. Apply refuses (ErrStale) if any of it differs.
	Token string `json:"token"`
}

// Result is what Apply did.
type Result struct {
	Plan           Plan     `json:"plan"`
	GraywolfErrors []string `json:"graywolf_errors,omitempty"`
}

// Preview works out what applying f as station would change.
func (s *Service) Preview(ctx context.Context, f *File, station string) (Plan, error) {
	p, _, err := s.plan(ctx, f, station)
	return p, err
}

func (s *Service) plan(ctx context.Context, f *File, station string) (Plan, gwPlan, error) {
	if !f.HasStation(station) {
		return Plan{}, gwPlan{}, invalid("no station %q in the file", station)
	}
	cur, err := s.st.GetSettings(ctx)
	if err != nil {
		return Plan{}, gwPlan{}, err
	}
	next, err := f.SettingsFor(station, cur)
	if err != nil {
		return Plan{}, gwPlan{}, err
	}
	p := Plan{Station: station, Role: next.Role, Settings: settingsChanges(cur, next), CanApply: true}
	if cur.RaceState != store.RaceSetup {
		p.CanApply = false
		p.Problems = append(p.Problems, "The race has started: a race config applies only before the race starts (or after a reset). Settings can still be edited on the Station page.")
	}
	if station == HQ && len(f.Checkpoints) > 0 {
		c, err := s.checkpointChange(ctx, f)
		if err != nil {
			return Plan{}, gwPlan{}, err
		}
		p.Checkpoints = c
	}
	if f.EventPage != "" {
		page, _, err := s.st.EventPage(ctx)
		if err != nil {
			return Plan{}, gwPlan{}, err
		}
		if page != f.EventPage {
			p.EventPage = &Change{Field: "event_page", Label: "Event page", From: pageSummary(page), To: pageSummary(f.EventPage)}
		}
	}
	gp := s.graywolfPlan(ctx, f, station, next)
	p.Graywolf = gp.changes
	for _, e := range gp.errs {
		p.Problems = append(p.Problems, "graywolf: "+e)
	}
	p.Token = token(f, p)
	return p, gp, nil
}

// token hashes the file and everything the preview showed.
func token(f *File, p Plan) string {
	b, _ := json.Marshal(struct {
		Digest, Station   string
		Settings          []Change
		Checkpoints, Page *Change
		Graywolf          []Change
	}{f.digest, p.Station, p.Settings, p.Checkpoints, p.EventPage, p.Graywolf})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

// Apply applies f as station, exactly as previewed (tok is the preview's
// token): the app's settings, HQ's checkpoint list and the event page in
// one transaction, then (if withGraywolf) the graywolf changes, after
// saving the original of each changed value for RestoreGraywolf. A
// graywolf failure doesn't undo the rest: each is reported. Everything
// stays editable on the device afterwards.
func (s *Service) Apply(ctx context.Context, f *File, station string, withGraywolf bool, tok string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, gp, err := s.plan(ctx, f, station)
	if err != nil {
		return Result{}, err
	}
	if !p.CanApply {
		return Result{Plan: p}, ErrNotSetup
	}
	if tok != p.Token {
		return Result{Plan: p}, ErrStale
	}
	cur, err := s.st.GetSettings(ctx)
	if err != nil {
		return Result{}, err
	}
	next, err := f.SettingsFor(station, cur)
	if err != nil {
		return Result{}, err
	}
	var list []store.Checkpoint
	if p.Checkpoints != nil {
		list = f.CheckpointList()
	}
	var page *string
	if p.EventPage != nil {
		page = &f.EventPage
	}
	if err := s.st.ApplyRaceConfig(ctx, next, list, page); err != nil {
		if errors.Is(err, store.ErrNotSetup) {
			return Result{Plan: p}, ErrNotSetup
		}
		return Result{}, err
	}
	res := Result{Plan: p}
	if withGraywolf && len(gp.changes) > 0 {
		res.GraywolfErrors = s.applyGraywolf(ctx, gp)
	}
	return res, nil
}

// gwBackup is the original of each graywolf value a config changed, as
// first seen: restore puts back only these fields. Nil = never changed.
type gwBackup struct {
	Callsign      *string `json:"callsign,omitempty"`
	TxTimingID    uint32  `json:"tx_timing_id,omitempty"`
	TxDelayMS     *int    `json:"tx_delay_ms,omitempty"`
	TxTailMS      *int    `json:"tx_tail_ms,omitempty"`
	RetentionDays *int    `json:"message_retention_days,omitempty"`
	Digipeater    *bool   `json:"digipeater,omitempty"`
}

// merge keeps b's values and adds o's for fields b doesn't have yet, so
// the first original of each value survives any number of applies.
func (b gwBackup) merge(o gwBackup) gwBackup {
	if b.Callsign == nil {
		b.Callsign = o.Callsign
	}
	if b.TxTimingID == 0 {
		b.TxTimingID = o.TxTimingID
	}
	if b.TxDelayMS == nil {
		b.TxDelayMS = o.TxDelayMS
	}
	if b.TxTailMS == nil {
		b.TxTailMS = o.TxTailMS
	}
	if b.RetentionDays == nil {
		b.RetentionDays = o.RetentionDays
	}
	if b.Digipeater == nil {
		b.Digipeater = o.Digipeater
	}
	return b
}

// gwPlan is a config's graywolf changes: what the preview shows, the
// originals (for the backup) and the records to write.
type gwPlan struct {
	changes  []Change
	orig     gwBackup
	callsign string
	timing   *graywolf.TxTiming
	prefs    *graywolf.MessagePreferences
	digi     *graywolf.Digipeater
	errs     []string
}

func (s *Service) graywolfPlan(ctx context.Context, f *File, station string, next store.Settings) gwPlan {
	var p gwPlan
	if call := f.StationCallsign(station); call != "" {
		sc, err := s.gw.StationConfig(ctx)
		switch {
		case err != nil:
			p.errs = append(p.errs, "reading the station callsign: "+err.Error())
		case !store.SameStation(sc.Callsign, call):
			orig := sc.Callsign
			p.orig.Callsign, p.callsign = &orig, call
			p.changes = append(p.changes, Change{"callsign", "Station callsign (graywolf, for all messaging)", sc.Callsign, call})
		}
	}
	g := f.Graywolf
	if g == nil {
		return p
	}
	if g.TxDelayMS != nil || g.TxTailMS != nil {
		tt, err := s.txTimingFor(ctx, next)
		if err != nil {
			p.errs = append(p.errs, "TX timing: "+err.Error())
		} else {
			nt := tt
			setInt(&nt.TxDelayMS, g.TxDelayMS)
			setInt(&nt.TxTailMS, g.TxTailMS)
			if nt.TxDelayMS != tt.TxDelayMS {
				d := tt.TxDelayMS
				p.orig.TxDelayMS = &d
				p.changes = append(p.changes, Change{"tx_delay_ms", fmt.Sprintf("TX delay, channel %d (ms)", tt.Channel), itoa(tt.TxDelayMS), itoa(nt.TxDelayMS)})
			}
			if nt.TxTailMS != tt.TxTailMS {
				t := tt.TxTailMS
				p.orig.TxTailMS = &t
				p.changes = append(p.changes, Change{"tx_tail_ms", fmt.Sprintf("TX tail, channel %d (ms)", tt.Channel), itoa(tt.TxTailMS), itoa(nt.TxTailMS)})
			}
			if nt != tt {
				p.orig.TxTimingID, p.timing = tt.ID, &nt
			}
		}
	}
	if g.RetentionDays != nil {
		pr, err := s.gw.MessagePreferences(ctx)
		switch {
		case err != nil:
			p.errs = append(p.errs, "reading message preferences: "+err.Error())
		case pr.RetentionDays != *g.RetentionDays:
			r := pr.RetentionDays
			np := pr
			np.RetentionDays = *g.RetentionDays
			p.orig.RetentionDays, p.prefs = &r, &np
			p.changes = append(p.changes, Change{"message_retention_days", "Message retention (days, 0 = forever)", itoa(pr.RetentionDays), itoa(np.RetentionDays)})
		}
	}
	if g.Digipeater != nil {
		d, err := s.gw.Digipeater(ctx)
		switch {
		case err != nil:
			p.errs = append(p.errs, "reading the digipeater: "+err.Error())
		case d.Enabled != *g.Digipeater:
			e := d.Enabled
			nd := d
			nd.Enabled = *g.Digipeater
			p.orig.Digipeater, p.digi = &e, &nd
			p.changes = append(p.changes, Change{"digipeater", "Digipeater", onOff(d.Enabled), onOff(nd.Enabled)})
		}
	}
	return p
}

// txTimingFor is the TX timing row of the channel the app sends on: the
// Messaging setting's channel, or graywolf's first enabled channel that
// can transmit.
func (s *Service) txTimingFor(ctx context.Context, cfg store.Settings) (graywolf.TxTiming, error) {
	ch := uint32(0)
	if cfg.GWChannel > 0 {
		ch = uint32(cfg.GWChannel)
	} else {
		chans, err := s.gw.Channels(ctx)
		if err != nil {
			return graywolf.TxTiming{}, err
		}
		for _, c := range chans {
			if c.Enabled && c.Backing.TX.Capable {
				ch = c.ID
				break
			}
		}
	}
	if ch == 0 {
		return graywolf.TxTiming{}, errors.New("graywolf has no enabled channel that can transmit")
	}
	tts, err := s.gw.TxTimings(ctx)
	if err != nil {
		return graywolf.TxTiming{}, err
	}
	for _, t := range tts {
		if t.Channel == ch {
			return t, nil
		}
	}
	return graywolf.TxTiming{}, fmt.Errorf("graywolf has no TX timing for the race's channel %d", ch)
}

// applyGraywolf saves the originals (merged into any earlier backup),
// then writes each changed record on its own.
func (s *Service) applyGraywolf(ctx context.Context, p gwPlan) []string {
	b, err := s.backup(ctx)
	if err != nil {
		return []string{"reading the saved graywolf settings (nothing changed): " + err.Error()}
	}
	data, _ := json.Marshal(b.merge(p.orig))
	if err := s.st.PutGraywolfBackup(ctx, string(data)); err != nil {
		return []string{"saving graywolf's current settings (nothing changed): " + err.Error()}
	}
	var out []string
	if p.callsign != "" {
		if _, err := s.gw.SetStationCallsign(ctx, p.callsign); err != nil {
			out = append(out, "station callsign: "+err.Error())
		}
	}
	if p.timing != nil {
		if _, err := s.gw.SetTxTiming(ctx, *p.timing); err != nil {
			out = append(out, "TX timing: "+err.Error())
		}
	}
	if p.prefs != nil {
		if _, err := s.gw.SetMessagePreferences(ctx, *p.prefs); err != nil {
			out = append(out, "message preferences: "+err.Error())
		}
	}
	if p.digi != nil {
		if _, err := s.gw.SetDigipeater(ctx, *p.digi); err != nil {
			out = append(out, "digipeater: "+err.Error())
		}
	}
	return out
}

func (s *Service) backup(ctx context.Context) (gwBackup, error) {
	var b gwBackup
	data, _, err := s.st.GraywolfBackup(ctx)
	if err != nil || data == "" {
		return b, err
	}
	if err := json.Unmarshal([]byte(data), &b); err != nil {
		return b, fmt.Errorf("saved graywolf settings unreadable: %w", err)
	}
	return b, nil
}

// HasGraywolfBackup reports whether graywolf settings can be restored.
func (s *Service) HasGraywolfBackup(ctx context.Context) (bool, error) {
	data, _, err := s.st.GraywolfBackup(ctx)
	return data != "", err
}

// RestoreGraywolf puts back each graywolf value a race config changed, as
// it was before the first change, and only those fields: anything else
// edited in graywolf since is left alone. If a write fails, the backup is
// kept so the restore can be retried. A callsign that was empty before
// can't be emptied through graywolf's API and is left as is (noted).
func (s *Service) RestoreGraywolf(ctx context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _, err := s.st.GraywolfBackup(ctx)
	if err != nil {
		return nil, err
	}
	if data == "" {
		return nil, ErrNoBackup
	}
	b, err := s.backup(ctx)
	if err != nil {
		return nil, fmt.Errorf("race config: %w", err)
	}
	var fails, notes []string
	if b.Callsign != nil {
		if *b.Callsign == "" {
			notes = append(notes, "graywolf had no station callsign before; its callsign was left as is")
		} else if _, err := s.gw.SetStationCallsign(ctx, *b.Callsign); err != nil {
			fails = append(fails, "station callsign: "+err.Error())
		}
	}
	if b.TxTimingID != 0 && (b.TxDelayMS != nil || b.TxTailMS != nil) {
		if err := s.restoreTxTiming(ctx, b); err != nil {
			fails = append(fails, "TX timing: "+err.Error())
		}
	}
	if b.RetentionDays != nil {
		pr, err := s.gw.MessagePreferences(ctx)
		if err == nil {
			pr.RetentionDays = *b.RetentionDays
			_, err = s.gw.SetMessagePreferences(ctx, pr)
		}
		if err != nil {
			fails = append(fails, "message preferences: "+err.Error())
		}
	}
	if b.Digipeater != nil {
		d, err := s.gw.Digipeater(ctx)
		if err == nil {
			d.Enabled = *b.Digipeater
			_, err = s.gw.SetDigipeater(ctx, d)
		}
		if err != nil {
			fails = append(fails, "digipeater: "+err.Error())
		}
	}
	if len(fails) > 0 {
		return notes, fmt.Errorf("race config: restoring graywolf settings: %s", strings.Join(fails, "; "))
	}
	return notes, s.st.ClearGraywolfBackup(ctx)
}

func (s *Service) restoreTxTiming(ctx context.Context, b gwBackup) error {
	tts, err := s.gw.TxTimings(ctx)
	if err != nil {
		return err
	}
	for _, t := range tts {
		if t.ID == b.TxTimingID {
			setInt(&t.TxDelayMS, b.TxDelayMS)
			setInt(&t.TxTailMS, b.TxTailMS)
			_, err := s.gw.SetTxTiming(ctx, t)
			return err
		}
	}
	return fmt.Errorf("the TX timing row it changed (id %d) is gone", b.TxTimingID)
}

func (s *Service) checkpointChange(ctx context.Context, f *File) (*Change, error) {
	cur, err := s.st.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	next := f.CheckpointList()
	if sameList(cur, next) {
		return nil, nil
	}
	return &Change{Field: "checkpoints", Label: "HQ's checkpoint list", From: listSummary(cur), To: listSummary(next)}, nil
}

func sameList(a, b []store.Checkpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Code != b[i].Code || a[i].Name != b[i].Name || a[i].CourseOrder != b[i].CourseOrder || a[i].ExpectedCall != b[i].ExpectedCall {
			return false
		}
	}
	return true
}

func listSummary(l []store.Checkpoint) string {
	if len(l) == 0 {
		return "(none)"
	}
	codes := make([]string, len(l))
	for i, c := range l {
		codes[i] = c.Code
	}
	return fmt.Sprintf("%d: %s", len(l), strings.Join(codes, ", "))
}

func pageSummary(p string) string {
	if p == "" {
		return "(none)"
	}
	first, _, _ := strings.Cut(strings.TrimSpace(p), "\n")
	return fmt.Sprintf("%d characters, starting %q", len([]rune(p)), first)
}

// settingsChanges lists the app settings that differ between cur and next.
func settingsChanges(cur, next store.Settings) []Change {
	type field struct {
		key, label string
		get        func(store.Settings) string
	}
	fields := []field{
		{"role", "Role", func(s store.Settings) string { return s.Role }},
		{"race_name", "Race name", func(s store.Settings) string { return s.RaceName }},
		{"station_tactical", "Station name", func(s store.Settings) string { return s.StationTactical }},
		{"checkpoint_code", "Checkpoint code", func(s store.Settings) string { return s.CheckpointCode }},
		{"hq_call", "HQ callsign", func(s store.Settings) string { return s.HQCall }},
		{"hq_local_codes", "Local codes (HQ)", func(s store.Settings) string { return s.HQLocalCodes }},
		{"path", "Digipeater path", func(s store.Settings) string { return s.Path }},
		{"gw_channel", "graywolf channel", func(s store.Settings) string { return itoa(s.GWChannel) }},
		{"heartbeat_sec", "Heartbeat every (s)", func(s store.Settings) string { return itoa(s.HeartbeatSec) }},
		{"flush_after_sec", "Batch after (s)", func(s store.Settings) string { return itoa(s.FlushAfterSec) }},
		{"max_in_flight", "Batches in flight", func(s store.Settings) string { return itoa(s.MaxInFlight) }},
		{"max_text_len", "Max message length", func(s store.Settings) string { return itoa(s.MaxTextLen) }},
		{"gap_grace_sec", "Gap grace (s)", func(s store.Settings) string { return itoa(s.GapGraceSec) }},
	}
	var out []Change
	for _, fl := range fields {
		if a, b := fl.get(cur), fl.get(next); a != b {
			out = append(out, Change{fl.key, fl.label, a, b})
		}
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}
