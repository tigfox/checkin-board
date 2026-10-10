package raceconfig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"checkin-board/internal/store"
)

// Export writes HQ's current setup as a race config file: race name, HQ
// (graywolf's callsign, station name, local codes), the checkpoint list,
// the messaging tunables (not the graywolf channel: channel ids are node
// hardware) and the event page. The graywolf section is left
// out (HQ's own TX timing shouldn't spread by default); add it by hand.
func (s *Service) Export(ctx context.Context) ([]byte, error) {
	cfg, err := s.st.GetSettings(ctx)
	if err != nil {
		return nil, err
	}
	if cfg.Role != store.RoleHQ {
		return nil, errors.New("race config: export is for HQ (it holds the checkpoint list)")
	}
	sc, err := s.gw.StationConfig(ctx)
	if err != nil {
		return nil, err
	}
	cps, err := s.st.ListCheckpoints(ctx)
	if err != nil {
		return nil, err
	}
	page, _, err := s.st.EventPage(ctx)
	if err != nil {
		return nil, err
	}
	f := File{
		Format: Format,
		Race:   Race{Name: cfg.RaceName},
		HQ:     HQStation{Callsign: strings.ToUpper(sc.Callsign), StationName: cfg.StationTactical},
		// No gw_channel: graywolf channel ids are node hardware.
		Messaging: &Messaging{Path: &cfg.Path, HeartbeatSec: &cfg.HeartbeatSec,
			FlushAfterSec: &cfg.FlushAfterSec, MaxInFlight: &cfg.MaxInFlight, MaxTextLen: &cfg.MaxTextLen, GapGraceSec: &cfg.GapGraceSec},
		EventPage: page,
	}
	if cfg.HQLocalCodes != "" {
		f.HQ.LocalCodes = strings.Split(cfg.HQLocalCodes, ",")
	}
	for _, c := range cps {
		f.Checkpoints = append(f.Checkpoints, Checkpoint{Code: c.Code, Name: c.Name, Callsign: c.ExpectedCall, CourseOrder: c.CourseOrder})
	}
	return json.MarshalIndent(f, "", "  ")
}
