// Package testnode runs whole checkin-board nodes in a test: app engines
// and the web API on a fake graywolf, served over HTTP, as the tools
// (benchrace, stationsetup) see a real node. For tests only.
package testnode

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"checkin-board/internal/app"
	"checkin-board/internal/auth"
	"checkin-board/internal/gwfake"
	"checkin-board/internal/radiobackend"
	"checkin-board/internal/radiokind"
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
	// Switchable gives the node both radios, graywolf and a UV-K5 (not
	// on a channel: Radio must be nil), and it restarts with the radio
	// kind chosen on its Station page, as a real node does. UVK5 says
	// which it starts with.
	Switchable bool
}

// Node is a running node.
type Node struct {
	URL   string
	Store *store.Store
	GW    *gwfake.Station // nil on a radio node
	UV    *uvk5.FakeRadio // a radio node's radio
	// RB is the radio backend the node started with (a restart into a
	// new radio kind makes another; read it only on a node that doesn't
	// switch).
	RB *radiobackend.Backend
}

// Start runs a node; everything stops when the test ends.
func Start(t testing.TB, opt Options) Node {
	t.Helper()
	if opt.Switchable && opt.Radio != nil {
		t.Fatal("testnode: a switchable node can't be on a channel")
	}
	st, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	var node Node
	if opt.UVK5 || opt.Switchable {
		node.UV = newFakeRadio(t, opt)
	}
	if !opt.UVK5 || opt.Switchable {
		node.GW = gwfake.New(opt.Call)
		if opt.Radio != nil {
			opt.Radio.Attach(node.GW)
		}
	}
	if opt.Settings != nil {
		if _, err := st.SaveSettings(ctx, *opt.Settings); err != nil {
			t.Fatal(err)
		}
	}
	au := auth.New(st, nil, 0)
	code, err := au.SetupCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := au.Setup(ctx, code, opt.Password); err != nil {
		t.Fatal(err)
	}
	r := &runner{t: t, opt: opt, st: st, au: au, node: &node, raceDir: t.TempDir()}
	kind := radiokind.Graywolf
	if opt.UVK5 {
		kind = radiokind.UVK5
	}
	if err := r.start(kind); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		h := r.handler.Load()
		if h == nil { // restarting
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		(*h).ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(r.close)
	node.URL, node.Store = srv.URL, st
	return node
}

// runner is a node's app, started (and restarted) with one radio kind.
type runner struct {
	t       testing.TB
	opt     Options
	st      *store.Store
	au      *auth.Service
	node    *Node
	raceDir string
	handler atomic.Pointer[http.Handler]

	mu       sync.Mutex
	closing  bool           // the test is ending: no more restarts
	restarts sync.WaitGroup // restarts in progress
	cancel   context.CancelFunc
	done     chan struct{}
	app      *app.App
}

var errClosing = errors.New("testnode: closing")

func (r *runner) start(kind string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return errClosing
	}
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := r.st.ActivateRadioKind(ctx, kind); err != nil {
		cancel()
		return err
	}
	var gw interface {
		app.Graywolf
		web.Graywolf
	}
	var rb *radiobackend.Backend
	backendDone := make(chan struct{})
	if kind == radiokind.UVK5 {
		var err error
		if rb, err = startBackend(ctx, r.node.UV, r.st, backendDone); err != nil {
			cancel()
			<-backendDone
			return err
		}
		r.node.RB, gw = rb, rb
	} else {
		close(backendDone)
		gw = r.node.GW
	}
	a, err := app.New(app.Config{Store: r.st, Graywolf: gw, DataDir: r.t.TempDir(), RadioKind: kind})
	if err != nil {
		cancel()
		<-backendDone
		return err
	}
	done := make(chan struct{})
	if r.opt.Run {
		go func() { defer close(done); _ = a.Run(ctx); <-backendDone }()
	} else {
		go func() { defer close(done); <-ctx.Done(); <-backendDone }()
	}
	deps := web.Deps{Store: r.st, Auth: r.au, Ops: a.Ops, HQ: a.HQ, Checkpoint: a.Checkpoint,
		Inbox: a.Inbox, Clock: a.Clock, Graywolf: gw, RaceConfigDir: r.raceDir,
		RadioKind: kind, GraywolfReady: r.node.GW != nil}
	if rb != nil {
		deps.Radio = rb
	}
	if r.opt.Switchable {
		deps.Restart = r.requestRestart
	}
	h, err := web.NewHandler(deps)
	if err != nil {
		cancel()
		<-done
		_ = a.Close()
		return err
	}
	r.cancel, r.done, r.app = cancel, done, a
	r.handler.Store(&h)
	return nil
}

func (r *runner) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.handler.Store(nil)
	if r.cancel != nil {
		r.cancel()
		<-r.done
		_ = r.app.Close()
		r.cancel = nil
	}
}

// close ends the node for good: no restart starts after it, and one in
// progress finishes first.
func (r *runner) close() {
	r.mu.Lock()
	r.closing = true
	r.mu.Unlock()
	r.restarts.Wait()
	r.stop()
}

// requestRestart is the web server asking for a restart (a new radio
// kind was chosen); it runs in the background, as the real app's does.
func (r *runner) requestRestart() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return
	}
	r.restarts.Add(1)
	go func() {
		defer r.restarts.Done()
		r.restart()
	}()
}

// restart is the app restarting with the radio kind chosen meanwhile.
func (r *runner) restart() {
	r.stop()
	kind, err := r.st.RadioKind(context.Background())
	if err == nil {
		err = r.start(kind)
	}
	if err != nil && !errors.Is(err, errClosing) {
		r.t.Errorf("testnode: restart: %v", err)
	}
}

// newFakeRadio runs a fake UV-K5 (on opt.Radio's channel, if any).
func newFakeRadio(t testing.TB, opt Options) *uvk5.FakeRadio {
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
	return uv
}

// startBackend runs the radio backend connected to uv over an in-memory
// serial line, until ctx ends (then done closes).
func startBackend(ctx context.Context, uv *uvk5.FakeRadio, st *store.Store, done chan struct{}) (*radiobackend.Backend, error) {
	rb, err := radiobackend.New(radiobackend.Config{Store: st, Dial: func(ctx context.Context) (radiobackend.Session, error) {
		host, dev := net.Pipe()
		go func() { _ = uv.Serve(dev) }()
		go func() { <-ctx.Done(); _ = dev.Close() }()
		return uvk5.Connect(ctx, host)
	}})
	if err != nil {
		close(done)
		return nil, err
	}
	go func() { defer close(done); rb.Run(ctx) }()
	for deadline := time.Now().Add(5 * time.Second); !rb.State().Connected; {
		if time.Now().After(deadline) {
			return nil, errors.New("testnode: the radio didn't connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
	return rb, nil
}

// radioListener puts a fake radio on the simulated channel.
type radioListener struct{ uv *uvk5.FakeRadio }

func (l radioListener) Callsign() string { return l.uv.Callsign() }
func (l radioListener) HearFrame(f gwfake.Frame) {
	l.uv.Hear(f.From, nil, uvk5.Message{Addressee: f.To, Text: f.Text, ID: f.MsgID, Ack: f.IsAck})
}
