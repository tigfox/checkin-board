package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
)

// Exit codes of `checkin-board linkcheck` (spec 4.8.7).
const (
	exitPass     = 0
	exitMarginal = 1
	exitFail     = 2 // FAIL, or the check couldn't run
)

// runLinkCheckCLI records a link-check request for the running service
// (through the database, so it needs the service user's file access,
// like reset-admin-password) and waits for the result.
func runLinkCheckCLI(ctx context.Context, dbPath string, args []string, out io.Writer, tm linkcheck.Timing) int {
	fs := flag.NewFlagSet("linkcheck", flag.ContinueOnError)
	fs.SetOutput(out)
	to := fs.String("to", "", "callsign to probe (HQ only; a checkpoint always probes its HQ)")
	count := fs.Int("count", 0, fmt.Sprintf("probes to send (default %d, max %d)", linkcheck.DefaultCount, linkcheck.MaxCount))
	asJSON := fs.Bool("json", false, "print the result as JSON")
	yes := fs.Bool("yes", false, "run even though the race is active (uses race airtime)")
	brief := fs.Bool("brief", false, "print one short line (for a graywolf Action's on-air reply)")
	db := fs.String("db", dbPath, "database path (default: CB_DB_PATH)")
	if err := fs.Parse(args); err != nil {
		return exitFail
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(out, "linkcheck: "+format+"\n", a...)
		return exitFail
	}
	st, err := store.Open(*db)
	if err != nil {
		return fail("open database %s: %v", *db, err)
	}
	defer st.Close()
	c, err := linkcheck.Request(ctx, st, linkcheck.Req{To: *to, Count: *count, Confirm: *yes, Source: "cli"}, time.Now())
	if errors.Is(err, linkcheck.ErrNeedsConfirm) {
		return fail("the race is active; add --yes to run a link check anyway")
	}
	if err != nil {
		return fail("%v", err)
	}
	if !*asJSON && !*brief {
		fmt.Fprintf(out, "Link check to %s: %d probes, %s apart…\n", c.PeerCall, c.Count, time.Duration(c.SpacingSec)*time.Second)
	}
	c, err = linkcheck.Await(ctx, st, c.ID, tm)
	if err != nil {
		return fail("%v", err)
	}
	if c.State == store.LinkCheckCancelled {
		return fail("cancelled: %s", c.Error)
	}
	switch {
	case *brief:
		fmt.Fprintln(out, linkcheck.Brief(c))
	case *asJSON:
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(cliResult(c)); err != nil {
			return fail("%v", err)
		}
	default:
		printLinkCheck(out, c)
	}
	switch c.Verdict {
	case linkcheck.Pass:
		return exitPass
	case linkcheck.Marginal:
		return exitMarginal
	}
	return exitFail
}

type cliLinkResult struct {
	Peer          string `json:"peer"`
	Verdict       string `json:"verdict"`
	Probes        int    `json:"probes"`
	Uplink        int    `json:"uplink"`
	RoundTrip     int    `json:"round_trip"`
	ReplyReceived bool   `json:"reply_received"`
	MedianRTTms   *int   `json:"median_rtt_ms,omitempty"`
	RemoteLevel   *int   `json:"remote_level_dbfs,omitempty"`
	LocalLevel    *int   `json:"local_level_dbfs,omitempty"`
	Via           string `json:"via,omitempty"`
	Advice        string `json:"advice,omitempty"`
	Error         string `json:"error,omitempty"`
}

func cliResult(c store.LinkCheck) cliLinkResult {
	return cliLinkResult{Peer: c.PeerCall, Verdict: c.Verdict, Probes: c.Count, Uplink: c.Uplink, RoundTrip: c.RoundTrip,
		ReplyReceived: c.ReplyReceived, MedianRTTms: c.MedianRTTms, RemoteLevel: c.RemoteLevel, LocalLevel: c.LocalLevel,
		Via: c.Via, Advice: c.Advice, Error: c.Error}
}

func printLinkCheck(out io.Writer, c store.LinkCheck) {
	lvl := func(v *int) string {
		if v == nil {
			return "n/a"
		}
		return fmt.Sprintf("%d dBFS", *v)
	}
	rtt := "n/a"
	if c.MedianRTTms != nil {
		rtt = fmt.Sprintf("%.1f s", float64(*c.MedianRTTms)/1000)
	}
	reply := "no"
	if c.ReplyReceived {
		reply = "yes"
	}
	via := c.Via
	if via == "" {
		via = "direct"
	}
	fmt.Fprintf(out, "%s to %s\n", c.Verdict, c.PeerCall)
	fmt.Fprintf(out, "  heard there %d/%d, ACKed %d/%d, reply %s, median round trip %s\n", c.Uplink, c.Count, c.RoundTrip, c.Count, reply, rtt)
	fmt.Fprintf(out, "  audio level there %s, here %s, path %s\n", lvl(c.RemoteLevel), lvl(c.LocalLevel), via)
	if c.Advice != "" {
		fmt.Fprintf(out, "  %s\n", strings.ToUpper(c.Advice[:1])+c.Advice[1:])
	}
	if c.Error != "" {
		fmt.Fprintf(out, "  note: %s\n", c.Error)
	}
}
