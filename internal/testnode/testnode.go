// Package testnode runs whole checkin-board nodes in a test: app engines
// and the web API on a fake graywolf, served over HTTP, as the tools
// (benchrace, stationsetup) see a real node. For tests only.
package testnode

import (
	"context"
	"net/http/httptest"
	"testing"

	"checkin-board/internal/app"
	"checkin-board/internal/auth"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/store"
	"checkin-board/internal/web"
)

// Options configure a node.
type Options struct {
	Call     string          // graywolf's station callsign
	Password string          // the admin password (first-run setup done)
	Settings *store.Settings // nil: a fresh node
	Radio    *gwfake.Radio   // nil: not on a channel
	Run      bool            // run the app's engines until the test ends
}

// Node is a running node.
type Node struct {
	URL   string
	Store *store.Store
	GW    *gwfake.Station
}

// Start runs a node; everything stops when the test ends.
func Start(t testing.TB, opt Options) Node {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	gw := gwfake.New(opt.Call)
	if opt.Radio != nil {
		opt.Radio.Attach(gw)
	}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if opt.Settings != nil {
		if _, err := st.SaveSettings(ctx, *opt.Settings); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.New(app.Config{Store: st, Graywolf: gw, DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if opt.Run {
		done := make(chan struct{})
		go func() { defer close(done); _ = a.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done })
	} else {
		t.Cleanup(cancel)
	}
	au := auth.New(st, nil, 0)
	code, err := au.SetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := au.Setup(ctx, code, opt.Password); err != nil {
		t.Fatal(err)
	}
	h, err := web.NewHandler(web.Deps{Store: st, Auth: au, Ops: a.Ops, HQ: a.HQ, Checkpoint: a.Checkpoint,
		Inbox: a.Inbox, Clock: a.Clock, Graywolf: gw, RaceConfigDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return Node{URL: srv.URL, Store: st, GW: gw}
}
