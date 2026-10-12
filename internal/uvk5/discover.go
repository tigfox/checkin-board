package uvk5

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
)

// portGlobs are where a radio on USB-C appears (a CDC ACM device).
var portGlobs = []string{"/dev/serial/by-id/*", "/dev/ttyACM*"}

// Candidates lists serial ports that could be the radio, most specific
// first, without duplicates.
func Candidates() []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range portGlobs {
		m, _ := filepath.Glob(g)
		sort.Strings(m)
		for _, p := range m {
			real, err := filepath.EvalSymlinks(p)
			if err != nil || seen[real] {
				continue
			}
			seen[real] = true
			out = append(out, p)
		}
	}
	return out
}

// Dial opens path, or (path "") the first candidate port that answers
// the handshake with suitable firmware.
func Dial(ctx context.Context, path string) (*Session, string, error) {
	ports := []string{path}
	if path == "" {
		ports = Candidates()
		if len(ports) == 0 {
			return nil, "", fmt.Errorf("uvk5: no radio found (no /dev/ttyACM*): is it plugged in by USB-C and switched on?")
		}
	}
	var lastErr error
	for _, p := range ports {
		port, err := OpenPort(p)
		if err != nil {
			lastErr = err
			continue
		}
		s, err := Connect(ctx, port)
		if err != nil {
			lastErr = fmt.Errorf("%s: %w", p, err)
			continue
		}
		return s, p, nil
	}
	return nil, "", lastErr
}
