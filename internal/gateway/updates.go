package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

type Options struct {
	PublicURL       string
	SuperuserKey    string
	Software        buildinfo.Info
	Apply           func(string) error
	ReleaseRepo     string
	ReleaseAPI      string
	ReleaseToken    string
	ReleaseInterval time.Duration
}

func (g *Gateway) receiveUpdateStatus(n model.Node, c *connection, data []byte) bool {
	var status update.Status
	if len(data) > 8192 || json.Unmarshal(data, &status) != nil {
		return false
	}
	d := g.updates.Current()
	status.SeenAt = time.Now().UTC()
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers[n.ID] != c || c.client {
		return false
	}
	previous := g.updateStatus[n.ID]
	latest := g.nodes[n.ID]
	before := latest
	if d != nil && status.ID == d.ID && status.State == "restarting" && (previous.ID != status.ID || previous.State != "restarting") {
		if asset, ok := d.Manifest.Asset(latest.Software.OS, latest.Software.Arch); ok && !update.IsCurrent(latest.Software, d.Manifest.Version, asset) {
			latest.UpdateUntil = status.SeenAt.Add(time.Minute)
		}
	}
	if updateAppliedAndResumed(d, status) {
		latest.UpdateUntil = time.Time{}
	}
	if !latest.UpdateUntil.Equal(before.UpdateUntil) {
		g.nodes[n.ID] = latest
		if err := g.persist(); err != nil {
			g.nodes[n.ID] = before
			return false
		}
	}
	g.updateStatus[n.ID] = status
	return true
}

func updateAppliedAndResumed(d *update.Deployment, status update.Status) bool {
	if d == nil || status.ID != d.ID || status.Paused {
		return false
	}
	asset, ok := d.Manifest.Asset(status.Software.OS, status.Software.Arch)
	return ok && update.IsCurrent(status.Software, d.Manifest.Version, asset)
}

// Called under g.mu. Display grace never changes routing or rollout readiness.
func (g *Gateway) nodeUpdating(n model.Node, now time.Time) bool {
	status := g.updateStatus[n.ID]
	d := g.updates.Current()
	fresh := n.Online && now.Sub(status.SeenAt) < 8*time.Second
	if fresh && updateAppliedAndResumed(d, status) {
		return false
	}
	if now.Before(n.UpdateUntil) {
		return true
	}
	if !fresh || d == nil || status.ID != d.ID {
		return false
	}
	switch status.State {
	case "downloading", "staged", "busy", "ready", "restarting":
		return true
	case "applied":
		return status.Paused
	}
	return false
}

func (g *Gateway) updateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/updates/blobs/{hash}", g.auth(func(w http.ResponseWriter, r *http.Request) {
		hash := r.PathValue("hash")
		if !update.ValidDigest(hash) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, g.updates.Blob(hash))
	}))
	mux.HandleFunc("PUT /v1/admin/updates/blobs/{hash}", g.admin(func(w http.ResponseWriter, r *http.Request) {
		asset := update.Asset{SHA256: r.PathValue("hash"), Size: r.ContentLength}
		if !update.ValidDigest(asset.SHA256) || asset.Size <= 0 || asset.Size > update.MaxBinarySize {
			http.Error(w, "invalid checksum or Content-Length", http.StatusBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, update.MaxBinarySize)
		if err := g.updates.Upload(r.Body, asset); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	mux.HandleFunc("POST /v1/admin/updates", g.admin(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var manifest update.Manifest
		if err := json.NewDecoder(r.Body).Decode(&manifest); err != nil {
			http.Error(w, "invalid manifest", http.StatusBadRequest)
			return
		}
		if err := g.validatePlatforms(manifest); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.rolloutMu.Lock()
		d, err := g.updates.Publish(manifest, "development")
		g.rolloutMu.Unlock()
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(d)
	}))
	mux.HandleFunc("GET /v1/admin/updates", g.admin(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		statuses := map[string]update.Status{}
		for id, status := range g.updateStatus {
			statuses[id] = status
		}
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"deployment": g.updates.Current(), "gateway": g.options.Software, "nodes": statuses})
	}))
	mux.HandleFunc("POST /v1/admin/updates/check", g.admin(func(w http.ResponseWriter, r *http.Request) {
		d, err := g.fetchRelease(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"deployment": d})
	}))
}

func (g *Gateway) validatePlatforms(manifest update.Manifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	if _, ok := manifest.Asset(g.options.Software.OS, g.options.Software.Arch); !ok {
		return errors.New("manifest must include the gateway platform")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, node := range g.nodes {
		arch := node.Software.Arch
		if arch == "" && node.System != nil {
			arch = node.System.Arch
		}
		if arch == "" {
			return fmt.Errorf("node %s does not report its architecture; upgrade it before managed rollout", node.Name)
		}
		if _, ok := manifest.Asset(node.OS, arch); !ok {
			return fmt.Errorf("manifest lacks %s/%s required by %s", node.OS, arch, node.Name)
		}
	}
	return nil
}

// StartUpdates begins managed rollout after the server is configured. A fresh
// heartbeat and an acknowledged idle reservation are required from every online
// peer before any binary replacement can start.
func (g *Gateway) StartUpdates(ctx context.Context) {
	if g.superuser == "" || g.options.Apply == nil {
		return
	}
	g.updateCtx, g.updateCancel = context.WithCancel(ctx)
	g.updateWG.Add(1)
	go func() { defer g.updateWG.Done(); g.rolloutLoop(g.updateCtx) }()
	if g.options.ReleaseRepo != "" {
		g.updateWG.Add(1)
		go func() {
			defer g.updateWG.Done()
			interval := g.options.ReleaseInterval
			if interval <= 0 {
				interval = 15 * time.Minute
			}
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				if _, err := g.fetchRelease(g.updateCtx); err != nil && g.updateCtx.Err() == nil {
					slog.Warn("release check", "error", err)
				}
				select {
				case <-g.updateCtx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
}

func (g *Gateway) control(id, kind string, command update.Command) {
	g.mu.Lock()
	peer := g.peers[id]
	g.mu.Unlock()
	if peer == nil || peer.client {
		return
	}
	select {
	case peer.out <- &protocol.Packet{Kind: kind, From: update.GatewaySender, Data: model.JSON(command)}:
	default:
	}
}

type rolloutPeer struct {
	node   model.Node
	status update.Status
	fresh  bool
}

func (g *Gateway) rolloutPeers(d *update.Deployment) []rolloutPeer {
	g.mu.Lock()
	defer g.mu.Unlock()
	peers := []rolloutPeer{}
	ids := map[string]bool{}
	for id, c := range g.peers {
		if !c.client {
			ids[id] = true
		}
	}
	if d.Phase == "installing" {
		for _, id := range d.Participants {
			ids[id] = true
		}
	}
	for id := range ids {
		if g.clientOwners[id] {
			continue
		}
		status, ok := g.updateStatus[id]
		peers = append(peers, rolloutPeer{g.nodes[id], status, g.peers[id] != nil && ok && time.Since(status.SeenAt) < 8*time.Second})
	}
	return peers
}

func (g *Gateway) stageGateway(ctx context.Context, d *update.Deployment) (string, error) {
	asset, ok := d.Manifest.Asset(g.options.Software.OS, g.options.Software.Arch)
	if !ok {
		return "", errors.New("gateway platform missing")
	}
	if update.IsCurrent(g.options.Software, d.Manifest.Version, asset) {
		return "", nil
	}
	path := filepath.Join(filepath.Dir(g.path), "updates", "staged", asset.SHA256, asset.File)
	if update.Verify(path, asset) != nil {
		f, err := os.Open(g.updates.Blob(asset.SHA256))
		if err != nil {
			return "", err
		}
		err = update.SaveBinary(f, path, asset)
		_ = f.Close()
		if err != nil {
			return "", err
		}
	}
	if err := update.ValidateExecutable(ctx, path, d.Manifest.Version, asset); err != nil {
		return "", err
	}
	return path, nil
}

func (g *Gateway) rolloutLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	var stagedID, stagedPath string
	var nextAttempt time.Time
	requestedRestart := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		func() {
			g.rolloutMu.Lock()
			defer g.rolloutMu.Unlock()
			d := g.updates.Current()
			if d == nil {
				return
			}
			if stagedID != d.ID {
				if time.Now().Before(nextAttempt) {
					return
				}
				path, err := g.stageGateway(ctx, d)
				if err != nil {
					_ = g.updates.SetPhase(d.ID, "error", err)
					nextAttempt = time.Now().Add(30 * time.Second)
					return
				}
				stagedID, stagedPath = d.ID, path
				requestedRestart = false
			}
			peers := g.rolloutPeers(d)
			allStaged, allIdle, allReady, peersApplied := true, true, true, true
			for _, peer := range peers {
				asset, ok := d.Manifest.Asset(peer.node.OS, peer.node.Software.Arch)
				if !ok {
					allStaged, peersApplied = false, false
					continue
				}
				command := update.Command{ID: d.ID, Version: d.Manifest.Version, Asset: asset}
				g.control(peer.node.ID, "update.offer", command)
				matching := peer.fresh && peer.status.ID == d.ID
				applied := matching && update.IsCurrent(peer.status.Software, d.Manifest.Version, asset)
				staged := matching && (applied || peer.status.State == "staged" || peer.status.State == "ready" || peer.status.State == "busy" || peer.status.State == "restarting")
				allStaged = allStaged && staged
				allIdle = allIdle && matching && !peer.status.Busy
				allReady = allReady && matching && peer.status.Paused && !peer.status.Busy && time.Until(peer.status.LeaseUntil) > 30*time.Second
				peersApplied = peersApplied && applied
			}
			if peersApplied && stagedPath == "" {
				for _, peer := range peers {
					g.control(peer.node.ID, "update.resume", update.Command{ID: d.ID})
				}
				if d.Phase != "complete" {
					_ = g.updates.SetPhase(d.ID, "complete", nil)
				}
				return
			}
			if !allStaged {
				if d.Phase != "installing" && d.Phase != "staging" {
					_ = g.updates.SetPhase(d.ID, "staging", nil)
				}
				return
			}
			if !allIdle {
				for _, peer := range peers {
					g.control(peer.node.ID, "update.resume", update.Command{ID: d.ID})
				}
				if d.Phase != "waiting-idle" && d.Phase != "installing" {
					_ = g.updates.SetPhase(d.ID, "waiting-idle", nil)
				}
				return
			}
			lease := time.Now().Add(2 * time.Minute).UTC()
			for _, peer := range peers {
				g.control(peer.node.ID, "update.prepare", update.Command{ID: d.ID, LeaseUntil: lease})
			}
			if !allReady {
				return
			}
			if d.Phase != "installing" {
				ids := []string{}
				for _, peer := range peers {
					ids = append(ids, peer.node.ID)
				}
				if err := g.updates.BeginInstall(d.ID, ids); err != nil {
					return
				}
			}
			for _, peer := range peers {
				g.control(peer.node.ID, "update.commit", update.Command{ID: d.ID})
			}
			// Keep the gateway available until every participating node has restarted
			// and reported the target version or checksum. Disconnections are never acknowledgments.
			if peersApplied && stagedPath != "" && !requestedRestart && g.reserveRestart(d, peers) {
				requestedRestart = true
				if err := g.options.Apply(stagedPath); err != nil {
					g.mu.Lock()
					g.restarting = false
					g.mu.Unlock()
					requestedRestart = false
					_ = g.updates.SetPhase(d.ID, "error", err)
				}
			}
		}()
	}
}

// Freeze registration atomically with the final idle check. A peer joining
// after the loop's snapshot must not start relay work during gateway shutdown.
func (g *Gateway) reserveRestart(d *update.Deployment, peers []rolloutPeer) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	machines := 0
	for _, c := range g.peers {
		if !c.client {
			machines++
		}
	}
	if machines != len(peers) {
		return false
	}
	for _, peer := range peers {
		status := g.updateStatus[peer.node.ID]
		asset, ok := d.Manifest.Asset(status.Software.OS, status.Software.Arch)
		if !ok || g.peers[peer.node.ID] == nil || status.ID != d.ID || !update.IsCurrent(status.Software, d.Manifest.Version, asset) || !status.Paused || status.Busy || time.Since(status.SeenAt) >= 8*time.Second || time.Until(status.LeaseUntil) <= 30*time.Second {
			return false
		}
	}
	g.restarting = true
	return true
}
