package node

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

func (n *Node) loadLease() error {
	err := store.Read(filepath.Join(n.Config.DataDir, "lease.json"), &n.lease)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Called with n.mu held. Expiry never permits overlapping a still-running task.
func (n *Node) leaseBusy() bool {
	if n.lease == nil {
		return false
	}
	for _, task := range n.tasks {
		if !task.Terminal() && task.Spec.LeaseID == n.lease.ID {
			return true
		}
	}
	return false
}

func (n *Node) leaseMethod(owner, method string, args json.RawMessage) (any, error) {
	var q struct {
		ID         string `json:"id"`
		TTLSeconds int    `json:"ttlSeconds"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	if q.TTLSeconds == 0 {
		q.TTLSeconds = 300
	}
	if q.TTLSeconds < 1 || q.TTLSeconds > 86400 {
		return nil, errors.New("lease TTL must be 1..86400 seconds")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if method == "leases.get" {
		if n.lease == nil {
			return nil, nil
		}
		return *n.lease, nil
	}
	var next *model.Lease
	switch method {
	case "leases.acquire":
		if n.activeInvocations > 0 {
			return nil, errors.New("node has active invocations")
		}
		if n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy()) {
			return nil, errors.New("node is already leased")
		}
		for _, task := range n.tasks {
			if !task.Terminal() {
				return nil, errors.New("node has active tasks")
			}
		}
		next = &model.Lease{ID: identity.NewID(), Owner: owner, Expires: time.Now().Add(time.Duration(q.TTLSeconds) * time.Second).UTC()}
	case "leases.renew", "leases.release":
		if n.lease == nil || n.lease.ID != q.ID || n.lease.Owner != owner {
			return nil, errors.New("lease not found or owned by another caller")
		}
		if method == "leases.renew" {
			if time.Now().After(n.lease.Expires) {
				return nil, errors.New("lease has expired")
			}
			copy := *n.lease
			copy.Expires = time.Now().Add(time.Duration(q.TTLSeconds) * time.Second).UTC()
			next = &copy
		} else if n.leaseBusy() {
			return nil, errors.New("lease still has active tasks")
		}
	}
	if err := store.Write(filepath.Join(n.Config.DataDir, "lease.json"), next); err != nil {
		return nil, err
	}
	n.lease = next
	if next == nil {
		return nil, nil
	}
	return *next, nil
}
