package web

import (
	"checkin-board/internal/hostmon"
)

// cableWarning and radioDownWarning are the keypad's and panel's lines
// (ASCII: the panel font).
const (
	cableWarning     = "Radio cable missing"
	radioDownWarning = "Radio not connected"
)

// CableStatus is the radio cable watcher (cablewatch.Watcher).
type CableStatus interface {
	Missing() []string
}

// hostSnapshot is the node's health. The sound cards are left out when
// graywolf runs on another machine: they aren't graywolf's.
func (s *server) hostSnapshot() hostmon.Snapshot {
	var h hostmon.Snapshot
	if s.Host != nil {
		h = s.Host.Snapshot()
	}
	if s.GraywolfRemote {
		h.SoundCards, h.SoundCardsKnown = nil, false
	}
	return h
}

// missingCards names the radio channel's sound cards that aren't on this
// node, from the watcher (nil: none, or no watcher).
func (s *server) missingCards() []string {
	if s.Cable == nil {
		return nil
	}
	return s.Cable.Missing()
}

// radioProblem is why race traffic can't go on air right now, from the
// node's own view ("": no problem it can see): a radio node's radio not
// connected, or graywolf's radio cable missing.
func (s *server) radioProblem() string {
	if s.Radio != nil {
		if !s.Radio.State().Connected {
			return radioDownWarning
		}
		return ""
	}
	if len(s.missingCards()) > 0 {
		return cableWarning
	}
	return ""
}

// backend names how this node sends race traffic.
func (s *server) backend() string {
	if s.Radio != nil {
		return "uvk5"
	}
	return "graywolf"
}
