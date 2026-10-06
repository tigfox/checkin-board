// Command checkin-board runs race checkpoint reporting next to a
// graywolf station, talking to it only through graywolf's REST API.
//
// Usage:
//
//	checkin-board                         run the service
//	checkin-board reset-admin-password    set a new admin password (reads it from stdin)
//	checkin-board version                 print the build version
//	checkin-board linkcheck [--to CALL] [--count N] [--json] [--yes]
//	                                      run a deployment link check through the
//	                                      running service; exit 0 PASS, 1 MARGINAL, 2 FAIL
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"checkin-board/internal/app"
	"checkin-board/internal/auth"
	"checkin-board/internal/config"
	"checkin-board/internal/graywolf"
	"checkin-board/internal/linkcheck"
	"checkin-board/internal/store"
	"checkin-board/internal/web"
)

// testedGraywolfVersion is the graywolf release this build was
// contract-tested against.
const testedGraywolfVersion = "0.14.14"

// version is set at build time: -ldflags "-X main.version=v1.0.0".
var version = "dev"

const (
	sessionPurgeEvery = time.Hour
	shutdownGrace     = 10 * time.Second
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var err error
	switch {
	case len(os.Args) > 1 && os.Args[1] == "reset-admin-password":
		err = resetAdminPassword(context.Background(), config.DBPathFrom(os.Getenv), os.Stdin, os.Stdout)
	case len(os.Args) > 1 && os.Args[1] == "version":
		printVersion(os.Stdout)
	case len(os.Args) > 1 && os.Args[1] == "linkcheck":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		code := runLinkCheckCLI(ctx, config.DBPathFrom(os.Getenv), os.Args[2:], os.Stdout, linkcheck.DefaultTiming)
		stop()
		os.Exit(code)
	default:
		err = run(logger)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// printVersion prints the build and the graywolf release it was tested with.
func printVersion(w io.Writer) {
	rev, dirty := "", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && len(s.Value) >= 8:
				rev = s.Value[:8]
			case s.Key == "vcs.modified" && s.Value == "true":
				dirty = "+dirty"
			}
		}
	}
	if rev != "" {
		rev = " (" + rev + dirty + ")"
	}
	fmt.Fprintf(w, "checkin-board %s%s, %s %s/%s, tested with graywolf %s\n",
		version, rev, runtime.Version(), runtime.GOOS, runtime.GOARCH, testedGraywolfVersion)
}

// resetAdminPassword sets a new admin password from the host (spec 7.2:
// a lost admin password). It needs shell access to the node, touches no
// race data, and logs every admin session out.
func resetAdminPassword(ctx context.Context, dbPath string, in io.Reader, out io.Writer) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database %s: %w", dbPath, err)
	}
	defer st.Close()
	fmt.Fprintf(out, "New admin password (%d+ characters): ", auth.MinAdminLen)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	pw := strings.TrimRight(line, "\r\n")
	if err := auth.New(st, nil, 0).ResetAdminPassword(ctx, pw); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nAdmin password changed; admin sessions logged out. Race data was not touched.")
	return nil
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startedAt := time.Now()
	logger.Info("checkin-board starting", "version", version, "tested_graywolf", testedGraywolfVersion)

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	logger.Info("starting", "config", cfg.String())

	gw, err := graywolf.New(graywolf.Config{
		BaseURL: cfg.GraywolfURL, Username: cfg.GraywolfUser, Password: cfg.GraywolfPassword,
		Timeout: cfg.Timeout, Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("create graywolf client: %w", err)
	}
	checkGraywolf(ctx, gw, logger)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	a, err := app.New(app.Config{
		Store: st, Graywolf: gw, Logger: logger, StartedAt: startedAt,
		DataDir: filepath.Dir(cfg.DBPath),
	})
	if err != nil {
		return fmt.Errorf("assemble app: %w", err)
	}
	defer a.Close()

	authSvc := auth.New(st, nil, 0)
	if code, err := authSvc.SetupCode(ctx); err != nil {
		return fmt.Errorf("setup code: %w", err)
	} else if code != "" {
		// Proves whoever sets the admin password can see this node's log.
		logger.Warn("first-run setup: open the web page and enter this setup code", "setup_code", code)
	}
	handler, err := web.NewHandler(web.Deps{
		Store: st, Auth: authSvc, Ops: a.Ops, HQ: a.HQ, Checkpoint: a.Checkpoint, Inbox: a.Inbox,
		Clock: a.Clock, Graywolf: gw, Logger: logger, HookToken: cfg.HookToken,
	})
	if err != nil {
		return fmt.Errorf("web handler: %w", err)
	}
	srv := &http.Server{
		Addr: cfg.Listen, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		logger.Info("web server listening", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()
	go purgeSessions(ctx, authSvc, logger)

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx) }()
	select {
	case err = <-errc:
		stop()
	case err = <-runErr:
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	return err
}

// purgeSessions deletes expired sessions hourly.
func purgeSessions(ctx context.Context, a *auth.Service, logger *slog.Logger) {
	t := time.NewTicker(sessionPurgeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.PurgeExpired(ctx); err != nil {
				logger.Warn("purge expired sessions", "err", err)
			}
		}
	}
}

// checkGraywolf logs graywolf's version and callsign. It never stops
// startup: graywolf may come up after us, and the engines retry.
func checkGraywolf(ctx context.Context, gw *graywolf.Client, logger *slog.Logger) {
	ver, err := gw.Version(ctx)
	if err != nil {
		logger.Warn("graywolf not reachable yet; will keep retrying", "err", err)
		return
	}
	if ver.Version != testedGraywolfVersion {
		logger.Warn("untested graywolf version", "have", ver.Version, "tested", testedGraywolfVersion)
	}
	station, err := gw.StationConfig(ctx)
	if err != nil {
		logger.Warn("read graywolf station config", "err", err)
		return
	}
	logger.Info("graywolf ready", "version", ver.Version, "callsign", station.Callsign)
}
