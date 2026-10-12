// Command benchrace runs a scripted race across checkin-board nodes and
// checks that HQ gets every passage exactly once (spec 9, phase 12 g).
// It is a test tool for the bench, not installed on nodes.
//
//	CB_BENCH_PASSWORD=... benchrace -hq http://10.0.0.65:8090 \
//	    -cp http://10.0.1.183:8090 [-cp ...] [-runners 100] [-duration 20m]
//
// Every node needs the same admin password, the race started at HQ and
// each checkpoint open. Bibs 9001-9100 (by default) must be unused: the
// bench logs real entries, so reset the race afterwards.
//
// Exit status: 0 every passage arrived exactly once, 1 not, 2 setup or
// usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"checkin-board/internal/bench"
)

// passwordEnv holds the nodes' admin password (not a flag: flags show in
// the process list).
const passwordEnv = "CB_BENCH_PASSWORD"

type urls []string

func (u *urls) String() string     { return strings.Join(*u, ",") }
func (u *urls) Set(v string) error { *u = append(*u, v); return nil }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("benchrace", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cps urls
	c := bench.Config{Out: stderr}
	fs.StringVar(&c.HQ, "hq", "", "HQ node URL, e.g. http://10.0.0.65:8090")
	fs.Var(&cps, "cp", "checkpoint node URL, in course order (repeat)")
	fs.IntVar(&c.Runners, "runners", 100, "runners")
	fs.IntVar(&c.FirstBib, "first-bib", 9001, "first bib (runners take consecutive bibs)")
	fs.DurationVar(&c.Duration, "duration", 20*time.Minute, "when the slowest runner reaches the last checkpoint")
	fs.DurationVar(&c.Settle, "settle", 15*time.Minute, "how long to wait for HQ after the last bib")
	fs.DurationVar(&c.PollEvery, "poll", 10*time.Second, "how often to read HQ's results")
	fs.Uint64Var(&c.Seed, "seed", 1, "plan seed (same seed, same race)")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "benchrace: unexpected arguments %q (each checkpoint needs its own -cp)\n", fs.Args())
		return 2
	}
	c.Checkpoints, c.Password = cps, getenv(passwordEnv)
	switch {
	case c.HQ == "" || len(cps) == 0:
		fmt.Fprintln(stderr, "benchrace: -hq and at least one -cp are required")
		return 2
	case c.Password == "":
		fmt.Fprintf(stderr, "benchrace: set %s to the nodes' admin password\n", passwordEnv)
		return 2
	}
	rep, err := bench.Run(ctx, c)
	if err != nil {
		fmt.Fprintln(stderr, "benchrace:", err)
		if errors.Is(err, bench.ErrSetup) {
			return 2
		}
		if rep.Expected == 0 {
			if errors.Is(err, bench.ErrInterrupted) {
				return 1
			}
			return 2
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(rep)
	} else {
		printReport(stdout, rep)
	}
	if err == nil && rep.OK() {
		return 0
	}
	return 1
}

func printReport(w io.Writer, r bench.Report) {
	verdict := "FAIL"
	if r.OK() {
		verdict = "PASS"
	}
	fmt.Fprintf(w, "%s: %d/%d passages at HQ", verdict, r.Delivered, r.Expected)
	if len(r.Duplicates) > 0 {
		fmt.Fprintf(w, ", %d duplicated", len(r.Duplicates))
	}
	if len(r.Unexpected) > 0 {
		fmt.Fprintf(w, ", %d unexpected", len(r.Unexpected))
	}
	fmt.Fprintf(w, "\nlatency (HQ received - checkpoint time in): %s\n", stats(r.Latency))
	if r.NegativeLatency > 0 {
		fmt.Fprintf(w, "  note: %d passages arrived \"before\" they were logged: the nodes' clocks disagree\n", r.NegativeLatency)
	}
	for _, cp := range r.PerCP {
		fmt.Fprintf(w, "  %-6s %d/%d  %s\n", cp.Code, cp.Delivered, cp.Expected, stats(cp.Latency))
	}
	for _, list := range []struct {
		name  string
		items []string
	}{{"missing", r.Missing}, {"duplicated", r.Duplicates}, {"unexpected", r.Unexpected},
		{"unconfirmed (no answer when logged, but at HQ)", r.Unconfirmed}} {
		for _, it := range list.items {
			fmt.Fprintf(w, "  %s: %s\n", list.name, it)
		}
	}
}

func stats(s bench.Stats) string {
	if s.N == 0 {
		return "no data"
	}
	return fmt.Sprintf("p50 %s, p95 %s, max %s", s.P50, s.P95, s.Max)
}
