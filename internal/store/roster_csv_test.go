package store

import (
	"os"
	"testing"
)

// The repo's example roster (README, "Roster") must load: leading zeros,
// a category column, and a name column that is discarded.
func TestExampleRoster(t *testing.T) {
	f, err := os.Open("../../example_roster.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	runners, err := ParseRosterCSV(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 20 || runners[0].Bib != 1 || runners[0].Category != "50K" || runners[19].Bib != 205 || runners[19].Category != "10K" {
		t.Fatalf("runners = %+v", runners)
	}
}
