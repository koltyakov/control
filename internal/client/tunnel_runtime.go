package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/koltyakov/control/internal/store"
)

// TunnelRuntime hosts retained tunnels inside the calling process. Unlike
// RunTunnelService it publishes no management endpoint, bearer token or saved
// profile: the account credentials stay in memory and only the host process
// can create, list or dispose tunnels. Retained definitions are restored when
// the runtime opens and their absolute expiry and disposal are preserved.
type TunnelRuntime struct {
	service *tunnelService
	lock    *flock.Flock
}

// OpenTunnelRuntime restores the retained tunnels of cfg's account login from
// a per-login subdirectory of root. Only one runtime or user tunnel service may
// own that directory; a directory with a saved user-service profile or
// management endpoint is rejected rather than shared. The caller owns root's
// access control. tunnels.json contains no credentials.
func OpenTunnelRuntime(ctx context.Context, root string, cfg StandaloneConfig) (*TunnelRuntime, error) {
	cfg.AccountRouting = true
	if cfg.Gateway.URL == "" || cfg.Gateway.Key == "" || cfg.StateDir == "" || root == "" {
		return nil, errors.New("tunnel runtime requires an account login, client state directory and tunnel directory")
	}
	dir, err := filepath.Abs(filepath.Join(root, tunnelScope(cfg)))
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "service.lock"))
	held, err := lock.TryLock()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	if !held {
		_ = lock.Close()
		return nil, errors.New("retained tunnels are already owned by another runtime or tunnel service")
	}
	for _, name := range []string{"service.json", "endpoint.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err == nil || !os.IsNotExist(err) {
			_ = lock.Close()
			if err == nil {
				err = errors.New("tunnel directory belongs to a user tunnel service; use a dedicated directory")
			}
			return nil, err
		}
	}
	// Check the saved state strictly before any listener is restored. The
	// legacy user service keeps its own, more permissive, restore checks.
	var infos []PersistentTunnelInfo
	if err = store.Read(filepath.Join(dir, "tunnels.json"), &infos); err != nil && !os.IsNotExist(err) {
		_ = lock.Close()
		return nil, err
	}
	if err = ValidateRetainedTunnelState(infos); err != nil {
		_ = lock.Close()
		return nil, err
	}
	m, err := newTunnelService(ctx, dir, cfg)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &TunnelRuntime{service: m, lock: lock}, nil
}

// Start durably accepts a retained tunnel. The context bounds target lookup
// only; the tunnel's lifetime belongs to the runtime.
func (t *TunnelRuntime) Start(ctx context.Context, spec PersistentTunnelSpec) (PersistentTunnelInfo, error) {
	return t.service.create(ctx, spec)
}

// List reports unexpired retained tunnels with their live state.
func (t *TunnelRuntime) List() []PersistentTunnelInfo { return t.service.list() }

// Dispose durably removes a retained tunnel, then closes its listener and
// sockets. It is idempotent for absent IDs.
func (t *TunnelRuntime) Dispose(id string) error {
	if err := ValidateTunnelID(id); err != nil {
		return err
	}
	return t.service.dispose(id)
}

// Close stops every retained listener without removing its definition, so a
// later runtime restores it. It releases directory ownership last.
func (t *TunnelRuntime) Close() {
	t.service.close()
	_ = t.lock.Close()
}

// MaxRetainedTunnels bounds the retained tunnels of one account login.
const MaxRetainedTunnels = maxForwards

// ValidateRetainedTunnelState strictly checks saved retained tunnel
// definitions, as written by a tunnel runtime or user tunnel service, before
// they are restored or imported. It requires at most MaxRetainedTunnels
// entries with unique IDs, a valid specification, a canonical target identity
// (64 lowercase hexadecimal characters), a creation time, and an expiry of
// exactly the creation time plus ttlSeconds, or zero without a TTL. Two
// entries may not claim the same listener. It does not check whether a
// definition has already expired; restoration disposes of those.
func ValidateRetainedTunnelState(infos []PersistentTunnelInfo) error {
	if len(infos) > MaxRetainedTunnels {
		return fmt.Errorf("retained tunnel state has %d entries; at most %d are allowed", len(infos), MaxRetainedTunnels)
	}
	ids := make(map[string]bool, len(infos))
	for i, info := range infos {
		invalid := func(reason string) error {
			return fmt.Errorf("invalid retained tunnel state entry %d: %s", i, reason)
		}
		if err := validatePersistentTunnel(info.PersistentTunnelSpec); err != nil {
			return invalid(err.Error())
		}
		if ids[info.ID] {
			return invalid("duplicate tunnel ID")
		}
		ids[info.ID] = true
		if !canonicalNodeID(info.NodeID) {
			return invalid("target must be a 64-character lowercase hexadecimal node ID")
		}
		if info.Created.IsZero() {
			return invalid("creation time is missing")
		}
		if info.TTLSeconds == 0 && !info.Expires.IsZero() || info.TTLSeconds > 0 && !info.Expires.Equal(info.Created.Add(time.Duration(info.TTLSeconds)*time.Second)) {
			return invalid("expiry must be zero without a TTL, or exactly the creation time plus ttlSeconds")
		}
		for _, other := range infos[:i] {
			if other.Listen == info.Listen && other.Reverse == info.Reverse && (!info.Reverse || other.NodeID == info.NodeID) {
				return invalid("another retained tunnel already uses that listener")
			}
		}
	}
	return nil
}

func canonicalNodeID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ValidateTunnelID checks a retained tunnel ID without a full specification.
func ValidateTunnelID(id string) error {
	return validatePersistentTunnel(PersistentTunnelSpec{ID: id, ForwardSpec: ForwardSpec{Node: "validate", Address: "127.0.0.1:1", Listen: "127.0.0.1:1"}})
}

// ValidatePersistentTunnel checks a retained tunnel specification before use.
func ValidatePersistentTunnel(spec PersistentTunnelSpec) error {
	return validatePersistentTunnel(spec)
}

// WithoutLocalRunbooks stops this client from reading project or global
// runbook stores of the host it runs on. Worker-store discovery through
// node.describe and runbooks.* calls is unaffected.
func (c Client) WithoutLocalRunbooks() Client {
	c.runbookRoots = []runbooks.Root{}
	return c
}
