package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// PersistentTunnelSpec describes a retained listener, not a replayable TCP stream.
type PersistentTunnelSpec struct {
	ForwardSpec
	ID         string `json:"id"`
	TTLSeconds int64  `json:"ttlSeconds,omitempty"`
}

type PersistentTunnelInfo struct {
	PersistentTunnelSpec
	NodeID    string    `json:"nodeId"`
	Created   time.Time `json:"created"`
	Expires   time.Time `json:"expires,omitempty"`
	State     string    `json:"state,omitempty"`
	Active    int       `json:"activeConnections"`
	LastError string    `json:"lastError,omitempty"`
}

type persistentAccess struct {
	root   string
	launch func(context.Context, string) error
}

type tunnelEndpoint struct {
	Address string `json:"address"`
	Token   string `json:"token"`
	PID     int    `json:"pid,omitempty"`
}

// WithPersistentTunnels enables host-owned tunnels without extending this client's lifetime.
// launch installs/starts a separate user service; no credentials are passed in argv.
func (c Client) WithPersistentTunnels(root string, launch func(context.Context, string) error) Client {
	c.persistent = &persistentAccess{root: root, launch: launch}
	return c
}

func tunnelScope(cfg StandaloneConfig) string {
	return identity.ID([]byte(strings.TrimRight(cfg.Gateway.URL, "/") + "\x00" + cfg.Gateway.Key))
}

func (c Client) tunnelDirectory() (string, error) {
	if c.persistent == nil || c.persistent.root == "" || c.routing == nil || !c.routing.cfg.AccountRouting {
		return "", errors.New("persistent tunnels require an account login and host tunnel service; explicit local API routing is unsupported")
	}
	return filepath.Abs(filepath.Join(c.persistent.root, tunnelScope(c.routing.cfg)))
}

func validatePersistentTunnel(spec PersistentTunnelSpec) error {
	if len(spec.ID) < 1 || len(spec.ID) > 128 {
		return errors.New("tunnel ID must have 1..128 ASCII letters, digits, underscores, or hyphens")
	}
	for _, r := range spec.ID {
		valid := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !valid {
			return errors.New("invalid tunnel ID")
		}
	}
	if spec.Node == "" || len(spec.Node) > 128 {
		return errors.New("a named target machine is required")
	}
	for _, address := range []string{spec.Address, spec.Listen} {
		host, port, err := net.SplitHostPort(address)
		n, parseErr := strconv.Atoi(port)
		if err != nil || parseErr != nil || host == "" || n < 1 || n > 65535 || len(address) > 512 {
			return errors.New("persistent tunnels require destination and listen HOST:PORT with a fixed port between 1 and 65535")
		}
	}
	if spec.TTLSeconds < 0 || spec.TTLSeconds > 365*24*60*60 {
		return errors.New("tunnel TTL must be zero for no expiry, or between 1s and 8760h")
	}
	return nil
}

func (c Client) StartPersistentTunnel(ctx context.Context, spec PersistentTunnelSpec) (PersistentTunnelInfo, error) {
	var info PersistentTunnelInfo
	if err := validatePersistentTunnel(spec); err != nil {
		return info, err
	}
	dir, err := c.tunnelDirectory()
	if err != nil {
		return info, err
	}
	if err = c.ensureTunnelService(ctx, dir, true); err != nil {
		return info, err
	}
	err = tunnelRequest(ctx, dir, http.MethodPost, "/tunnels", spec, &info)
	return info, err
}

func (c Client) PersistentTunnels(ctx context.Context) ([]PersistentTunnelInfo, error) {
	infos := []PersistentTunnelInfo{}
	dir, err := c.tunnelDirectory()
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(filepath.Join(dir, "service.json")); os.IsNotExist(err) {
		return infos, nil
	} else if err != nil {
		return nil, err
	}
	if err = c.ensureTunnelService(ctx, dir, false); err != nil {
		return nil, err
	}
	err = tunnelRequest(ctx, dir, http.MethodGet, "/tunnels", nil, &infos)
	return infos, err
}

func (c Client) DisposePersistentTunnel(ctx context.Context, id string) error {
	// IDs are also URL path components. Never accept a path supplied by the caller.
	if err := validatePersistentTunnel(PersistentTunnelSpec{ID: id, ForwardSpec: ForwardSpec{Node: "validate", Address: "127.0.0.1:1", Listen: "127.0.0.1:1"}}); err != nil {
		return err
	}
	dir, err := c.tunnelDirectory()
	if err != nil {
		return err
	}
	if _, err = os.Stat(filepath.Join(dir, "service.json")); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err = c.ensureTunnelService(ctx, dir, false); err != nil {
		return err
	}
	return tunnelRequest(ctx, dir, http.MethodDelete, "/tunnels/"+id, nil, nil)
}

func (c Client) ensureTunnelService(ctx context.Context, dir string, create bool) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := tunnelRequest(ctx, dir, http.MethodGet, "/health", nil, nil); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "startup.lock"))
	defer func() { _ = lock.Close() }()
	held, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return err
	}
	if !held {
		return errors.New("timed out starting tunnel service")
	}
	if err := tunnelRequest(ctx, dir, http.MethodGet, "/health", nil, nil); err == nil {
		return nil
	}
	path := filepath.Join(dir, "service.json")
	var saved StandaloneConfig
	if err = store.Read(path, &saved); os.IsNotExist(err) && create {
		role, authErr := c.routing.cfg.Gateway.AuthRole(ctx)
		if authErr != nil {
			return authErr
		}
		if role != "user" && role != "superuser" {
			return errors.New("persistent tunnels require account credentials")
		}
		saved = c.routing.cfg
		saved.StateDir, err = filepath.Abs(saved.StateDir)
		if err != nil {
			return err
		}
		if err = store.Write(path, saved); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if c.persistent.launch == nil {
		return errors.New("tunnel service launcher is not configured")
	}
	// A stopped management API does not mean its owner has released the runtime
	// lock. Wait for shutdown before launching a process that would otherwise exit.
	serviceLock := flock.New(filepath.Join(dir, "service.lock"))
	defer func() { _ = serviceLock.Close() }()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		// An owner that is still starting may become ready without stopping.
		if err := tunnelRequest(ctx, dir, http.MethodGet, "/health", nil, nil); err == nil {
			return nil
		}
		held, err = serviceLock.TryLock()
		if err != nil {
			return err
		}
		if held {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tunnel service did not stop or become ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
	if err = serviceLock.Unlock(); err != nil {
		return err
	}
	if err = c.persistent.launch(ctx, dir); err != nil {
		return err
	}
	for {
		if err := tunnelRequest(ctx, dir, http.MethodGet, "/health", nil, nil); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("tunnel service not ready; inspect %s: %w", filepath.Join(dir, "service.log"), ctx.Err())
		case <-ticker.C:
		}
	}
}

func tunnelRequest(ctx context.Context, dir, method, path string, params, result any) error {
	var endpoint tunnelEndpoint
	if err := store.Read(filepath.Join(dir, "endpoint.json"), &endpoint); err != nil {
		return err
	}
	host, _, err := net.SplitHostPort(endpoint.Address)
	if err != nil || host != "127.0.0.1" || endpoint.Token == "" {
		return errors.New("invalid local tunnel service endpoint")
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+endpoint.Address+path, bytes.NewReader(model.JSON(params)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+endpoint.Token)
	req.Header.Set("Content-Type", "application/json")
	h := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer h.CloseIdleConnections()
	resp, err := h.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("tunnel service %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(result)
}

// Dashboard reads the atomically written definitions without starting the service
// or opening peer connections. Only aggregate counts enter the fleet snapshot.
func (c Client) addTunnelCounts(pool *model.PoolActivitySnapshot) {
	dir, err := c.tunnelDirectory()
	if err != nil {
		return
	}
	var infos []PersistentTunnelInfo
	if err = store.Read(filepath.Join(dir, "tunnels.json"), &infos); err != nil && !os.IsNotExist(err) {
		return
	}
	counts := map[string]model.TunnelCounts{}
	for _, info := range infos {
		if !info.Expires.IsZero() && !time.Now().Before(info.Expires) {
			continue
		}
		count := counts[info.NodeID]
		if info.Reverse {
			count.Reverse++
		} else {
			count.Forward++
		}
		counts[info.NodeID] = count
	}
	for i := range pool.Nodes {
		count := counts[pool.Nodes[i].ID]
		pool.Nodes[i].RetainedTunnels = &count
	}
}
