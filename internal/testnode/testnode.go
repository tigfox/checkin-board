// Package testnode runs whole checkin-board nodes in a test: app engines
// and the web API on a fake graywolf, served over HTTP, as the tools
// (benchrace, stationsetup) see a real node. For tests only.
package testnode

import (
	"context"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"checkin-board/internal/app"
	"checkin-board/internal/auth"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/store"
	"checkin-board/internal/uvk5"
	"checkin-board/internal/web"
)

// Options configure a node.
type Options struct {
	Call     string          // graywolf's station callsign
	Password string          // the admin password (first-run setup done)
	Settings *store.Settings // nil: a fresh node
	Radio    *gwfake.Radio   // nil: not on a channel
	Run      bool            // run the app's engines until the test ends
	// UVK5 makes it a radio node (phase 17): a fake UV-K5 radio with the
	// agreed firmware, on Radio's channel, behind the radio backend.
	UVK5 bool
}

// Node is a running node.
type Node struct {
	URL   string
	Store *store.Store
	GW    *gwfake.Station // nil on a radio node
	UV    *uvk5.FakeRadio // a radio node's radio
	RB    *radiobackend.Backend
}

// Start runs a node; everything stops when the test ends.
func Start(t testing.TB, opt Options) Node {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var node Node
	var gw interface {
		app.Graywolf
		web.Graywolf
	}
	if opt.UVK5 {
		node.UV, node.RB = startRadio(ctx, t, opt, st)
		gw = node.RB
	} else {
		node.GW = gwfake.New(opt.Call)
		if opt.Radio != nil {
			opt.Radio.Attach(node.GW)
		}
		gw = node.GW
	}
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
	deps := web.Deps{Store: st, Auth: au, Ops: a.Ops, HQ: a.HQ, Checkpoint: a.Checkpoint,
		Inbox: a.Inbox, Clock: a.Clock, Graywolf: gw, RaceConfigDir: t.TempDir()}
	if node.RB != nil {
		deps.Radio = node.RB
	}
	h, err := web.NewHandler(deps)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	node.URL, node.Store = srv.URL, st
	return node
}

// startRadio runs a fake radio (on opt.Radio's channel, if any) and the
// radio backend connected to it over an in-memory serial line.
func startRadio(ctx context.Context, t testing.TB, opt Options, st *store.Store) (*uvk5.FakeRadio, *radiobackend.Backend) {
	t.Helper()
	uv := uvk5.NewFakeRadio(opt.Call, time.Now)
	if opt.Radio != nil {
		ch := opt.Radio
		uv.Air = func(from, to string, m uvk5.Message) {
			ch.Transmit(gwfake.Frame{From: from, To: to, Text: m.Text, MsgID: m.ID, IsAck: m.Ack})
		}
		ch.AttachListener(radioListener{uv})
	}
	stop := make(chan struct{})
	go uv.Run(100*time.Millisecond, stop)
	t.Cleanup(func() { close(stop) })
	rb, err := radiobackend.New(radiobackend.Config{Store: st, Dial: func(ctx context.Context) (radiobackend.Session, error) {
		host, dev := net.Pipe()
		go func() { _ = uv.Serve(dev) }()
		go func() { <-ctx.Done(); _ = dev.Close() }()
		return uvk5.Connect(ctx, host)
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); rb.Run(ctx) }()
	t.Cleanup(func() { <-done })
	for deadline := time.Now().Add(5 * time.Second); !rb.State().Connected; {
		if time.Now().After(deadline) {
			t.Fatal("testnode: the radio didn't connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return uv, rb
}

// radioListener puts a fake radio on the simulated channel.
type radioListener struct{ uv *uvk5.FakeRadio }

func (l radioListener) Callsign() string { return l.uv.Callsign() }
func (l radioListener) HearFrame(f gwfake.Frame) {
	l.uv.Hear(f.From, nil, uvk5.Message{Addressee: f.To, Text: f.Text, ID: f.MsgID, Ack: f.IsAck})
}
