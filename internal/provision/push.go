package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"checkin-board/internal/nodeapi"
	"checkin-board/internal/raceconfig"
)

// Options configure a setup run.
type Options struct {
	Password string // admin password, the same on every node
	// Graywolf also applies the graywolf part on each node: its station
	// callsign and the race's TX timing. graywolf's original values are
	// kept on the node for Restore graywolf settings.
	Graywolf bool
	// RestartWait bounds the wait for a node to restart with a new radio
	// type (0: DefaultRestartWait).
	RestartWait time.Duration
}

// DefaultRestartWait is how long a node gets to restart with a new radio
// type: the app restarts in seconds, but a Pi Zero can be slow.
const DefaultRestartWait = 2 * time.Minute

// restartPoll is how often a restarting node is asked whether it's back.
const restartPoll = time.Second

// Node is one target's progress: its preview, then what applying did.
type Node struct {
	Target
	Preview        raceconfig.Plan
	Err            error // reaching, uploading, previewing or applying
	Applied        bool
	Unsure         bool // the apply got no answer: it may have happened
	GraywolfErrors []string
	// SwitchFrom is the node's radio type when the CSV's backend column
	// asks for another: applying switches it first (the node restarts).
	SwitchFrom string
	Switched   bool // the radio type was switched
	client     *nodeapi.Client
}

type radioKinds struct {
	Current string `json:"current"`
	Kinds   []struct {
		ID        string `json:"id"`
		Available bool   `json:"available"`
		Why       string `json:"why"`
	} `json:"kinds"`
}

// Ready is whether the node can be set up (preview fine).
func (n *Node) Ready() bool { return n.Err == nil && n.Preview.CanApply }

// FileName is the name the race config gets on each node: from a hash of
// its contents, so it never replaces anyone's own file, and the same CSV
// always gives the same file.
func (p *Plan) FileName() string {
	sum := sha256.Sum256(p.Config)
	return "stationsetup-" + hex.EncodeToString(sum[:4]) + ".json"
}

// PreviewAll logs in to every node, uploads the race config (unless the
// node already has it) and previews it for that node's station. Nothing
// on a node changes except that file in its race config folder. Nodes
// are contacted in parallel.
func PreviewAll(ctx context.Context, p *Plan, opt Options) []*Node {
	nodes := make([]*Node, len(p.Targets))
	var wg sync.WaitGroup
	for i, t := range p.Targets {
		nodes[i] = &Node{Target: t, client: nodeapi.New(t.Address)}
		wg.Go(func() { nodes[i].Err = preview(ctx, nodes[i], p, opt) })
	}
	wg.Wait()
	return nodes
}

func preview(ctx context.Context, n *Node, p *Plan, opt Options) error {
	if err := n.client.Login(ctx, opt.Password); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if n.Backend != "" {
		if err := checkRadioKind(ctx, n); err != nil {
			return err
		}
	}
	return previewOnly(ctx, n, p)
}

// checkRadioKind sees whether the node can use the CSV's radio type,
// and notes a switch when it uses another now.
func checkRadioKind(ctx context.Context, n *Node) error {
	var rk radioKinds
	if err := n.client.Do(ctx, http.MethodGet, "/api/admin/radio-kind", nil, &rk); err != nil {
		if nodeapi.StatusOf(err) == http.StatusNotFound {
			return errors.New("this node's checkin-board can't change its radio type: update it first")
		}
		return fmt.Errorf("radio type: %w", err)
	}
	for _, k := range rk.Kinds {
		if k.ID != n.Backend {
			continue
		}
		if !k.Available {
			return fmt.Errorf("radio type %s isn't available on this node: %s", n.Backend, k.Why)
		}
		if rk.Current != n.Backend {
			n.SwitchFrom = rk.Current
		}
		return nil
	}
	return fmt.Errorf("this node doesn't know radio type %s: update it first", n.Backend)
}

// previewOnly uploads the race config if needed and previews it.
func previewOnly(ctx context.Context, n *Node, p *Plan) error {
	// A 409 means the file is already there: the name is its hash.
	err := n.client.Upload(ctx, "/api/admin/raceconfigs", p.FileName(), p.Config, nil, nil)
	if err != nil && nodeapi.StatusOf(err) != http.StatusConflict {
		return fmt.Errorf("upload: %w", err)
	}
	path := "/api/admin/raceconfigs/" + url.PathEscape(p.FileName()) + "/preview"
	if err := n.client.Do(ctx, http.MethodPost, path, map[string]string{"station": n.Station}, &n.Preview); err != nil {
		return fmt.Errorf("preview: %w", err)
	}
	return nil
}

// ApplyAll applies the previewed config on every ready node, exactly as
// previewed (the node refuses if anything changed since). Nodes that
// aren't ready are skipped.
func ApplyAll(ctx context.Context, p *Plan, nodes []*Node, opt Options) {
	var wg sync.WaitGroup
	for _, n := range nodes {
		if !n.Ready() {
			continue
		}
		wg.Go(func() {
			if n.SwitchFrom != "" {
				if err := switchKind(ctx, n, p, opt); err != nil {
					n.Err = err
					return
				}
			}
			var res raceconfig.Result
			path := "/api/admin/raceconfigs/" + url.PathEscape(p.FileName()) + "/apply"
			body := map[string]any{"station": n.Station, "graywolf": opt.Graywolf, "token": n.Preview.Token}
			if err := n.client.Do(ctx, http.MethodPost, path, body, &res); err != nil {
				if nodeapi.StatusOf(err) == 0 {
					// No answer: the node may have applied it anyway.
					n.Unsure = true
					n.Err = fmt.Errorf("apply: no clear answer (%w); run again to see whether it was set up", err)
					return
				}
				n.Err = fmt.Errorf("apply: %w", err)
				return
			}
			n.Applied, n.GraywolfErrors = true, res.GraywolfErrors
		})
	}
	wg.Wait()
}

// switchKind switches the node's radio type, waits for it to restart
// with it, and previews again: what loading the config changes depends
// on the radio type.
func switchKind(ctx context.Context, n *Node, p *Plan, opt Options) error {
	var out struct {
		Restarting bool `json:"restarting"`
	}
	// No answer (the node restarting as it replied): the poll below sees
	// whether the switch happened.
	if err := n.client.Do(ctx, http.MethodPut, "/api/admin/radio-kind", map[string]string{"kind": n.Backend}, &out); err != nil &&
		nodeapi.StatusOf(err) != 0 {
		return fmt.Errorf("switch radio type to %s: %w", n.Backend, err)
	}
	wait := opt.RestartWait
	if wait <= 0 {
		wait = DefaultRestartWait
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		var rk radioKinds
		err := n.client.Do(wctx, http.MethodGet, "/api/admin/radio-kind", nil, &rk)
		if err == nil && rk.Current == n.Backend {
			break
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("radio type %s chosen, but the node didn't come back with it within %s: check the node, then run again", n.Backend, wait)
		case <-time.After(restartPoll):
		}
	}
	n.Switched = true
	if err := previewOnly(ctx, n, p); err != nil {
		return fmt.Errorf("after switching the radio type: %w", err)
	}
	if !n.Preview.CanApply {
		return fmt.Errorf("after switching the radio type, the node can't load the config: %s", strings.Join(n.Preview.Problems, "; "))
	}
	return nil
}
