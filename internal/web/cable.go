package web

import (
	"checkin-board/internal/hostmon"
)

// cableWarning is the panel's line (ASCII: the panel font).
const cableWarning = "Radio cable missing"

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
