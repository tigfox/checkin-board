// Command stationsetup sets up every node of a race from one stations
// CSV, over the local network: one line per node with its address,
// role, names, codes and callsign, plus race-wide messaging and graywolf
// settings. See README.md, "Setting up every node from a CSV", and
// example_stations.csv.
//
//	CB_ADMIN_PASSWORD=... stationsetup [-dry-run] [-yes] [-no-graywolf] stations.csv
//
// Each node gets the CSV as a race config file, shows what loading it
// would change, and after one confirmation loads it for its own station.
// Nodes must be in race setup (not started). Exit status: 0 every node
// set up, 1 some weren't, 2 a usage or CSV error (no node touched).
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"checkin-board/internal/provision"
	"checkin-board/internal/raceconfig"
)

// passwordEnv holds the nodes' admin password (not a flag: flags show
// in the process list).
const passwordEnv = "CB_ADMIN_PASSWORD"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("stationsetup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "usage: %s=... stationsetup [flags] stations.csv\n", passwordEnv)
		fs.PrintDefaults()
	}
	dryRun := fs.Bool("dry-run", false, "only show what each node would change")
	yes := fs.Bool("yes", false, "apply without asking")
	noGW := fs.Bool("no-graywolf", false, "leave graywolf alone (callsign, TX timing)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	opt := provision.Options{Password: getenv(passwordEnv), Graywolf: !*noGW}
	if opt.Password == "" {
		fmt.Fprintf(stderr, "stationsetup: set %s to the nodes' admin password\n", passwordEnv)
		return 2
	}
	f, err := os.Open(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "stationsetup:", err)
		return 2
	}
	plan, err := provision.Parse(f)
	f.Close()
	if err != nil {
		fmt.Fprintln(stderr, "stationsetup:", err)
		return 2
	}

	fmt.Fprintf(stdout, "Race %q: %d nodes (file %s on each)\n\n", plan.RaceName, len(plan.Targets), plan.FileName())
	nodes := provision.PreviewAll(ctx, plan, opt)
	ready := 0
	for i, n := range nodes {
		printPreview(stdout, i+1, n, opt.Graywolf)
		if n.Ready() {
			ready++
		}
	}
	switch {
	case ready == 0:
		fmt.Fprintln(stdout, "No node can be set up.")
		return 1
	case *dryRun:
		fmt.Fprintf(stdout, "Dry run: %d of %d nodes ready; nothing changed.\n", ready, len(nodes))
		return exitFor(nodes, false)
	case !*yes && !confirm(stdin, stdout, ready, len(nodes)):
		fmt.Fprintln(stdout, "Nothing changed.")
		return 1
	}
	provision.ApplyAll(ctx, plan, nodes, opt)
	fmt.Fprintln(stdout)
	for i, n := range nodes {
		printResult(stdout, i+1, n)
	}
	return exitFor(nodes, true)
}

func printPreview(w io.Writer, i int, n *provision.Node, graywolf bool) {
	what := "HQ"
	if n.Station != raceconfig.HQ {
		what = "checkpoint " + n.Station
	}
	fmt.Fprintf(w, "[%d] %s (%s) at %s\n", i, n.Label, what, n.Address)
	if n.Err != nil {
		fmt.Fprintf(w, "    ERROR: %v\n\n", n.Err)
		return
	}
	p := n.Preview
	if n.SwitchFrom != "" {
		fmt.Fprintf(w, "    %-22s %s -> %s (the node restarts first; the changes below are checked again for the new radio)\n",
			"Radio type:", n.SwitchFrom, n.Backend)
	}
	for _, c := range p.Settings {
		fmt.Fprintf(w, "    %-22s %s -> %s\n", c.Label+":", orNone(c.From), orNone(c.To))
	}
	if p.Checkpoints != nil {
		fmt.Fprintf(w, "    %-22s %s -> %s\n", p.Checkpoints.Label+":", orNone(p.Checkpoints.From), orNone(p.Checkpoints.To))
	}
	if p.EventPage != nil {
		fmt.Fprintf(w, "    %-22s %s -> %s\n", p.EventPage.Label+":", orNone(p.EventPage.From), orNone(p.EventPage.To))
	}
	for _, c := range p.Graywolf {
		tag := "graywolf: "
		if !graywolf {
			tag = "graywolf (skipped): "
		}
		fmt.Fprintf(w, "    %s%s: %s -> %s\n", tag, c.Label, orNone(c.From), orNone(c.To))
	}
	if n.SwitchFrom == "" && len(p.Settings) == 0 && p.Checkpoints == nil && p.EventPage == nil && len(p.Graywolf) == 0 {
		fmt.Fprintln(w, "    (already set up: nothing to change)")
	}
	for _, pr := range p.Problems {
		fmt.Fprintf(w, "    PROBLEM: %s\n", pr)
	}
	if !p.CanApply && len(p.Problems) == 0 {
		fmt.Fprintln(w, "    PROBLEM: the node can't load it now")
	}
	fmt.Fprintln(w)
}

func printResult(w io.Writer, i int, n *provision.Node) {
	if n.Switched {
		fmt.Fprintf(w, "[%d] %s: radio type now %s\n", i, n.Label, n.Backend)
	}
	switch {
	case n.Applied && len(n.GraywolfErrors) == 0:
		fmt.Fprintf(w, "[%d] %s: set up\n", i, n.Label)
	case n.Applied:
		fmt.Fprintf(w, "[%d] %s: set up, but graywolf: %s\n", i, n.Label, strings.Join(n.GraywolfErrors, "; "))
	case n.Unsure:
		fmt.Fprintf(w, "[%d] %s: OUTCOME UNKNOWN: %v\n", i, n.Label, n.Err)
	case n.Err != nil:
		fmt.Fprintf(w, "[%d] %s: NOT set up: %v\n", i, n.Label, n.Err)
	default:
		fmt.Fprintf(w, "[%d] %s: NOT set up (see its problems above)\n", i, n.Label)
	}
}

func confirm(stdin io.Reader, stdout io.Writer, ready, total int) bool {
	fmt.Fprintf(stdout, "Set up %d of %d nodes as shown? Type yes to continue: ", ready, total)
	line, _ := bufio.NewReader(stdin).ReadString('\n')
	return strings.EqualFold(strings.TrimSpace(line), "yes")
}

// exitFor is 0 when every node is (or would be) set up cleanly.
func exitFor(nodes []*provision.Node, applied bool) int {
	for _, n := range nodes {
		if !n.Ready() || (applied && (!n.Applied || len(n.GraywolfErrors) > 0)) {
			return 1
		}
	}
	return 0
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
