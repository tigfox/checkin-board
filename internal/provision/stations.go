// Package provision sets up every node of a race from one stations CSV
// (stationsetup): one line per node, with its address, role, names,
// codes and callsign, plus race-wide messaging and graywolf settings.
// The CSV becomes a race config file (phase 12a), which each node
// previews and applies for its own station, so every rule and safeguard
// of loading a race config applies: validation, setup-only, preview
// then apply, and graywolf's original values kept for Restore.
package provision

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"checkin-board/internal/nodeapi"
	"checkin-board/internal/raceconfig"
	"checkin-board/internal/store"
)

// ErrCSV wraps every problem with the stations CSV.
var ErrCSV = errors.New("stations CSV")

// Columns, in the order the example uses. address, role, station_name,
// code (checkpoints) and callsign are required; blank optional cells
// leave the node's current value alone.
var Columns = []string{
	"address", "role", "station_name", "code", "callsign", "course_order",
	"race_name", "checkpoint_name",
	"path", "heartbeat_sec", "flush_after_sec", "max_in_flight", "max_text_len", "gap_grace_sec", "gw_channel",
	"tx_delay_ms", "tx_tail_ms", "backend",
}

var required = []string{"address", "role", "station_name", "code", "callsign"}

// raceWide columns must agree on every row that sets them.
var raceWide = []string{"race_name", "path", "heartbeat_sec", "flush_after_sec", "max_in_flight",
	"max_text_len", "gap_grace_sec", "gw_channel", "tx_delay_ms", "tx_tail_ms"}

// Target is one node to set up: where it is and which station it is.
type Target struct {
	Line    int    // CSV line, for messages
	Address string // as written in the CSV
	Station string // the race config's station id: raceconfig.HQ or a checkpoint code
	Role    string
	Label   string
	// Backend is what the node must be ("graywolf" or "uvk5"; "": either).
	Backend string
}

// Plan is a parsed stations CSV: the race config every node loads, and
// the nodes to load it on.
type Plan struct {
	RaceName string
	Config   []byte // the race config file (JSON), validated
	Targets  []Target
}

// Parse reads a stations CSV and builds the race config from it.
func Parse(r io.Reader) (*Plan, error) {
	rows, header, err := readCSV(r)
	if err != nil {
		return nil, err
	}
	shared, err := raceWideValues(rows, header)
	if err != nil {
		return nil, err
	}
	f := raceconfig.File{Format: raceconfig.Format, Race: raceconfig.Race{Name: shared["race_name"]}}
	if f.Messaging, err = messaging(shared); err != nil {
		return nil, err
	}
	if f.Graywolf, err = graywolfPart(shared); err != nil {
		return nil, err
	}
	var targets []Target
	seenAddr := map[string]int{}
	for _, row := range rows {
		get := func(col string) string { return row.cells[col] }
		if strings.Contains(get("address"), "@") {
			return nil, lineErr(row.line, "address %q: give the node's IP or name only (no user or password)", get("address"))
		}
		key := strings.ToLower(nodeapi.BaseURL(get("address")))
		if prev, ok := seenAddr[key]; ok {
			return nil, lineErr(row.line, "address %q is the same node as line %d", get("address"), prev)
		}
		seenAddr[key] = row.line
		t := Target{Line: row.line, Address: get("address"), Role: strings.ToLower(get("role")), Label: get("station_name"),
			Backend: strings.ToLower(get("backend"))}
		if t.Backend != "" && t.Backend != "graywolf" && t.Backend != "uvk5" {
			return nil, lineErr(row.line, "backend %q must be graywolf or uvk5", get("backend"))
		}
		switch t.Role {
		case store.RoleHQ:
			if f.HQ.Callsign != "" {
				return nil, lineErr(row.line, "a second HQ row (only one node can be HQ)")
			}
			f.HQ = raceconfig.HQStation{Callsign: get("callsign"), StationName: get("station_name"), LocalCodes: splitCodes(get("code"))}
			t.Station = raceconfig.HQ
		case store.RoleCheckpoint:
			order := 0
			if v := get("course_order"); v != "" {
				if order, err = strconv.Atoi(v); err != nil || order < 1 {
					return nil, lineErr(row.line, "course_order %q must be a whole number from 1", v)
				}
			}
			name := cmpOr(get("checkpoint_name"), get("station_name"))
			f.Checkpoints = append(f.Checkpoints, raceconfig.Checkpoint{Code: get("code"), Name: name,
				StationName: get("station_name"), Callsign: get("callsign"), CourseOrder: order})
			t.Station = strings.ToUpper(strings.TrimSpace(get("code")))
		default:
			return nil, lineErr(row.line, "role %q must be hq or checkpoint", get("role"))
		}
		targets = append(targets, t)
	}
	if f.HQ.Callsign == "" {
		return nil, fmt.Errorf("%w: no HQ row (one node must have role hq)", ErrCSV)
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, err
	}
	// Validate exactly as each node will.
	parsed, err := raceconfig.Parse(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCSV, err)
	}
	for i := range targets {
		if !parsed.HasStation(targets[i].Station) {
			return nil, lineErr(targets[i].Line, "station %q isn't in the race config", targets[i].Station)
		}
	}
	return &Plan{RaceName: f.Race.Name, Config: data, Targets: targets}, nil
}

type csvRow struct {
	line  int
	cells map[string]string
}

// maxCSVBytes caps the stations CSV (a few hundred nodes at most).
const maxCSVBytes = 256 << 10

func readCSV(r io.Reader) ([]csvRow, []string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxCSVBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrCSV, err)
	}
	if len(data) > maxCSVBytes {
		return nil, nil, fmt.Errorf("%w: larger than %d KB", ErrCSV, maxCSVBytes>>10)
	}
	data = bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF")) // a spreadsheet's byte-order mark
	cr := csv.NewReader(bytes.NewReader(data))
	cr.Comment = '#'
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, nil, fmt.Errorf("%w: header: %v", ErrCSV, err)
	}
	for i := range header {
		header[i] = strings.ToLower(strings.TrimSpace(header[i]))
		if !slices.Contains(Columns, header[i]) {
			return nil, nil, fmt.Errorf("%w: unknown column %q (columns: %s)", ErrCSV, header[i], strings.Join(Columns, ", "))
		}
		if slices.Contains(header[:i], header[i]) {
			return nil, nil, fmt.Errorf("%w: column %q appears twice", ErrCSV, header[i])
		}
	}
	for _, col := range required {
		if !slices.Contains(header, col) {
			return nil, nil, fmt.Errorf("%w: missing column %q", ErrCSV, col)
		}
	}
	var rows []csvRow
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ErrCSV, err)
		}
		line, _ := cr.FieldPos(0)
		cells := map[string]string{}
		blank := true
		for i, v := range rec {
			cells[header[i]] = strings.TrimSpace(v)
			blank = blank && cells[header[i]] == ""
		}
		if blank {
			continue
		}
		for _, col := range required {
			if cells[col] == "" && !(col == "code" && strings.EqualFold(cells["role"], store.RoleHQ)) {
				return nil, nil, lineErr(line, "%s is empty", col)
			}
		}
		rows = append(rows, csvRow{line: line, cells: cells})
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("%w: no stations", ErrCSV)
	}
	return rows, header, nil
}

func raceWideValues(rows []csvRow, header []string) (map[string]string, error) {
	out := map[string]string{}
	from := map[string]int{}
	for _, col := range raceWide {
		if !slices.Contains(header, col) {
			continue
		}
		for _, row := range rows {
			v := row.cells[col]
			if v == "" {
				continue
			}
			if prev, ok := out[col]; ok && canonical(col, prev) != canonical(col, v) {
				return nil, lineErr(row.line, "%s %q differs from line %d's %q: it's race-wide, so set it once or the same on every row", col, v, from[col], prev)
			}
			out[col], from[col] = v, row.line
		}
	}
	return out, nil
}

// canonical is a race-wide value as the node will see it, so "300" and
// "0300", or "WIDE1-1, WIDE2-1" and "wide1-1,wide2-1", agree.
func canonical(col, v string) string {
	switch col {
	case "race_name":
		return v
	case "path":
		return strings.ToUpper(strings.ReplaceAll(v, " ", ""))
	}
	if n, err := strconv.Atoi(v); err == nil {
		return strconv.Itoa(n)
	}
	return v
}

func messaging(v map[string]string) (*raceconfig.Messaging, error) {
	var m raceconfig.Messaging
	set := false
	if p, ok := v["path"]; ok {
		path := p
		if strings.EqualFold(p, "direct") {
			path = "" // no digipeaters
		}
		m.Path, set = &path, true
	}
	for _, f := range []struct {
		col string
		dst **int
	}{{"heartbeat_sec", &m.HeartbeatSec}, {"flush_after_sec", &m.FlushAfterSec}, {"max_in_flight", &m.MaxInFlight},
		{"max_text_len", &m.MaxTextLen}, {"gap_grace_sec", &m.GapGraceSec}, {"gw_channel", &m.GWChannel}} {
		n, ok, err := intCell(v, f.col)
		if err != nil {
			return nil, err
		}
		if ok {
			*f.dst, set = &n, true
		}
	}
	if !set {
		return nil, nil
	}
	return &m, nil
}

func graywolfPart(v map[string]string) (*raceconfig.Graywolf, error) {
	var g raceconfig.Graywolf
	set := false
	for _, f := range []struct {
		col string
		dst **int
	}{{"tx_delay_ms", &g.TxDelayMS}, {"tx_tail_ms", &g.TxTailMS}} {
		n, ok, err := intCell(v, f.col)
		if err != nil {
			return nil, err
		}
		if ok {
			*f.dst, set = &n, true
		}
	}
	if !set {
		return nil, nil
	}
	return &g, nil
}

func intCell(v map[string]string, col string) (int, bool, error) {
	s, ok := v[col]
	if !ok {
		return 0, false, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %s %q must be a whole number", ErrCSV, col, s)
	}
	return n, true, nil
}

// splitCodes reads HQ's local codes: "START;FIN" (or spaces).
func splitCodes(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ' ' || r == '|' })
}

func lineErr(line int, format string, args ...any) error {
	return fmt.Errorf("%w: line %d: %s", ErrCSV, line, fmt.Sprintf(format, args...))
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
