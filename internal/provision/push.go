package provision

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sync"

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
}

// Node is one target's progress: its preview, then what applying did.
type Node struct {
	Target
	Preview        raceconfig.Plan
	Err            error // reaching, uploading, previewing or applying
	Applied        bool
	Unsure         bool // the apply got no answer: it may have happened
	GraywolfErrors []string
	client         *nodeapi.Client
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
		var set struct {
			Backend string `json:"backend"`
		}
		if err := n.client.Do(ctx, http.MethodGet, "/api/admin/settings", nil, &set); err != nil {
			return fmt.Errorf("settings: %w", err)
		}
		if set.Backend != "" && set.Backend != n.Backend {
			return fmt.Errorf("the CSV says backend %s, but this node uses %s (is the address right?)", n.Backend, set.Backend)
		}
	}
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
