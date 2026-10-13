// Package radiokind lists the kinds of radio a node can send race
// traffic through. The node's kind is chosen on the Station page (or by
// stationsetup) and can change mid-race when a radio is swapped: the app
// restarts with the new kind, and nothing is lost.
//
// To add a radio: give it an entry here, a backend that answers the
// app's graywolf-shaped messaging calls (see internal/radiobackend), and
// a case in cmd/checkin-board's startMessaging.
package radiokind

import "strings"

// Kind ids, as saved in the database and used in the API and CSVs.
const (
	Graywolf = "graywolf"
	UVK5     = "uvk5"
)

// Default is a new node's kind.
const Default = Graywolf

// Kind is one kind of radio.
type Kind struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	// Help says what the operator connects, for the Station page.
	Help string `json:"help"`
	// NeedsGraywolf: the kind sends through graywolf, which must be
	// installed with the app's login (install.sh does both).
	NeedsGraywolf bool `json:"needs_graywolf"`
}

var kinds = []Kind{
	{ID: Graywolf, Label: "Radio with an AIOC cable (graywolf)",
		Help:          "Any 2 m radio on an AIOC (or other sound-card) cable; graywolf on this node is the modem.",
		NeedsGraywolf: true},
	{ID: UVK5, Label: "UV-K5 V3 / UV-K1 on USB-C (APRS firmware)",
		Help: "A Quansheng UV-K5 V3 or UV-K1 running the APRS messaging firmware, by its USB-C cable; the radio is the modem."},
}

// All returns every kind, in the order the Station page lists them.
func All() []Kind {
	return append([]Kind(nil), kinds...)
}

// Lookup finds a kind by id (case-insensitive).
func Lookup(id string) (Kind, bool) {
	for _, k := range kinds {
		if strings.EqualFold(k.ID, strings.TrimSpace(id)) {
			return k, true
		}
	}
	return Kind{}, false
}

// IDs lists the ids, for error messages.
func IDs() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.ID
	}
	return out
}
