package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

const maxTrackedActivities = 2048
const maxRecentActivities = 64

type activityContextKey struct{}
type activityContext struct{ owner, task string }

type activityHandle struct {
	node     *Node
	ctx      context.Context
	info     model.Activity // Protected by node.activityMu.
	bytes    atomic.Int64
	sent     atomic.Int64
	received atomic.Int64
	once     sync.Once
	tracked  bool
}

func (n *Node) beginActivity(ctx context.Context, kind, operation, owner, peer string) *activityHandle {
	meta, _ := ctx.Value(activityContextKey{}).(activityContext)
	if owner == "" {
		owner = meta.owner
	}
	if owner == "" {
		owner = n.Identity.ID
	}
	now := time.Now().UTC()
	h := &activityHandle{node: n, ctx: ctx, info: model.Activity{ID: identity.NewID(), Kind: kind, Operation: bounded(operation, 128), Owner: owner, Peer: bounded(peer, 128), TaskID: meta.task, State: "running", Started: now, Updated: now}}
	n.activityMu.Lock()
	if len(n.activities) < maxTrackedActivities {
		h.tracked = true
		n.activities[h.info.ID] = h
	} else {
		n.untrackedActivities++
	}
	n.activityMu.Unlock()
	return h
}

func (h *activityHandle) phase(phase string) {
	h.node.activityMu.Lock()
	h.info.Phase, h.info.Updated = phase, time.Now().UTC()
	h.node.activityMu.Unlock()
}

func (h *activityHandle) progress(offset, total int64) {
	h.node.activityMu.Lock()
	h.info.TotalBytes = total
	h.node.activityMu.Unlock()
	h.bytes.Store(offset)
}

// snapshot is called with activityMu held.
func (h *activityHandle) snapshot() model.Activity {
	a := h.info
	a.BytesDone, a.BytesSent, a.BytesReceived = h.bytes.Load(), h.sent.Load(), h.received.Load()
	return a
}

func (h *activityHandle) finish(err error) {
	h.once.Do(func() {
		h.node.activityMu.Lock()
		defer h.node.activityMu.Unlock()
		if !h.tracked {
			h.node.untrackedActivities--
			return
		}
		delete(h.node.activities, h.info.ID)
		a := h.snapshot()
		a.Finished, a.Updated, a.State = time.Now().UTC(), time.Now().UTC(), "succeeded"
		if err != nil {
			a.State = "failed"
			if h.ctx.Err() != nil {
				a.State = "cancelled"
			}
		}
		h.node.recentActivities = append(h.node.recentActivities, a)
		if len(h.node.recentActivities) > maxRecentActivities {
			h.node.recentActivities = h.node.recentActivities[len(h.node.recentActivities)-maxRecentActivities:]
		}
	})
}

type activityWriter struct {
	writer  io.Writer
	counter *atomic.Int64
}

func (w activityWriter) Write(b []byte) (int, error) {
	n, err := w.writer.Write(b)
	w.counter.Add(int64(n))
	return n, err
}

type activityConn struct {
	net.Conn
	activity *activityHandle
}

func (c *activityConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.activity.received.Add(int64(n))
	return n, err
}
func (c *activityConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.activity.sent.Add(int64(n))
	return n, err
}
func (c *activityConn) Close() error { err := c.Conn.Close(); c.activity.finish(err); return err }

func trackOperation(method string) bool {
	// Exclude observation requests so the dashboard never monitors its own polls.
	switch method {
	case "nodes.list", "nodes.select", "node.describe", "system.info", "capabilities.list", "activities.list", "activities.pool", "tasks.start", "tasks.get", "tasks.list", "tasks.logs", "leases.get", "artifacts.list", "artifacts.pull":
		return false
	default:
		return true
	}
}

func (n *Node) taskPhase(id, phase string) {
	n.mu.Lock()
	if task := n.tasks[id]; task != nil {
		task.Phase, task.Updated = phase, time.Now().UTC()
	}
	n.mu.Unlock()
}

func taskActivity(t *model.Task) model.Activity {
	a := model.Activity{ID: t.ID, TaskID: t.ID, Kind: "task", Operation: bounded(t.Spec.Capability, 128), Owner: t.Owner, State: t.State, Phase: t.Phase, Started: t.Created, Updated: t.Updated}
	if t.Terminal() {
		a.Finished = t.Updated
	}
	return a
}

func (n *Node) activitySnapshot(query model.ActivityQuery) model.NodeActivitySnapshot {
	s := model.NodeActivitySnapshot{ID: n.Identity.ID, Name: n.Config.Name, OS: runtime.GOOS, Online: true, Status: "ready", ObservedAt: time.Now().UTC(), Active: []model.Activity{}, Recent: []model.Activity{}}
	s.Software, s.Disabled = buildinfo.Current(), n.currentMachineState().Disabled
	n.activityMu.Lock()
	for _, h := range n.activities {
		s.Active = append(s.Active, h.snapshot())
	}
	s.Omitted = n.untrackedActivities
	if query.Recent > 0 {
		s.Recent = append(s.Recent, n.recentActivities...)
	}
	n.activityMu.Unlock()
	n.mu.Lock()
	for _, task := range n.tasks {
		if !task.Terminal() {
			s.Active = append(s.Active, taskActivity(task))
		} else if query.Recent > 0 {
			s.Recent = append(s.Recent, taskActivity(task))
		}
	}
	if n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy()) {
		s.LeaseOwner, s.LeaseExpires = n.lease.Owner, n.lease.Expires
	}
	n.mu.Unlock()
	for _, mode := range n.Peer.Connections() {
		if mode == "webrtc" {
			s.DirectSessions++
		} else {
			s.RelaySessions++
		}
	}
	sort.Slice(s.Active, func(i, j int) bool {
		if s.Active[i].Started.Equal(s.Active[j].Started) {
			return s.Active[i].ID < s.Active[j].ID
		}
		return s.Active[i].Started.Before(s.Active[j].Started)
	})
	sort.Slice(s.Recent, func(i, j int) bool { return s.Recent[i].Finished.After(s.Recent[j].Finished) })
	if len(s.Recent) > query.Recent {
		s.Recent = s.Recent[:query.Recent]
	}
	s.ActiveCount = len(s.Active) + s.Omitted
	info := n.system.Snapshot()
	s.System = &info
	return s
}

func decodeActivityQuery(args json.RawMessage) (model.ActivityQuery, error) {
	var q model.ActivityQuery
	err := json.Unmarshal(args, &q)
	if err == nil && (q.Recent < 0 || q.Recent > maxRecentActivities) {
		err = fmt.Errorf("recent must be 0..%d", maxRecentActivities)
	}
	return q, err
}

// Pool snapshots run with the local node's identity. A remote caller cannot use
// the aggregator to bypass an individual node's activities.list authorization.
func (n *Node) poolActivities(ctx context.Context, caller string, args json.RawMessage) (model.PoolActivitySnapshot, error) {
	var q model.PoolActivityQuery
	if err := json.Unmarshal(args, &q); err != nil {
		return model.PoolActivitySnapshot{}, err
	}
	if caller != n.Identity.ID {
		return model.PoolActivitySnapshot{}, errors.New("activities.pool is available through the local API only")
	}
	if _, err := decodeActivityQuery(args); err != nil {
		return model.PoolActivitySnapshot{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	registered, err := n.Peer.Nodes(ctx)
	if err != nil {
		return model.PoolActivitySnapshot{}, err
	}
	selected := map[string]bool{}
	for _, name := range q.Nodes {
		selected[name] = false
	}
	pool := model.PoolActivitySnapshot{Nodes: []model.NodeActivitySnapshot{}}
	for _, peer := range registered {
		_, byID := selected[peer.ID]
		_, byName := selected[peer.Name]
		if len(selected) > 0 && !byID && !byName {
			continue
		}
		if byID {
			selected[peer.ID] = true
		}
		if byName {
			selected[peer.Name] = true
		}
		pool.Nodes = append(pool.Nodes, model.NodeActivitySnapshot{ID: peer.ID, Name: peer.Name, OS: peer.OS, Labels: peer.Labels, Online: peer.Online, LastSeen: peer.LastSeen, Software: peer.Software, Disabled: peer.Disabled, ControlPending: peer.ControlPending, Status: "offline", Active: []model.Activity{}, Recent: []model.Activity{}, System: peer.System})
	}
	for name, found := range selected {
		if !found {
			return pool, fmt.Errorf("unknown node %q", name)
		}
	}
	var wg sync.WaitGroup
	jobs := make(chan int, len(pool.Nodes))
	for i := range pool.Nodes {
		if pool.Nodes[i].Online {
			jobs <- i
		}
	}
	close(jobs)
	for range min(8, len(pool.Nodes)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := &pool.Nodes[i]
				var remote model.NodeActivitySnapshot
				pollCtx, release := context.WithTimeout(ctx, 6*time.Second)
				err := n.Call(pollCtx, s.ID, "activities.list", model.ActivityQuery{Recent: q.Recent}, &remote)
				release()
				if err != nil {
					s.Status, s.Error = "unavailable", bounded(err.Error(), 256)
					continue
				}
				s.Status, s.ObservedAt, s.Active, s.Recent = "ready", remote.ObservedAt, remote.Active, remote.Recent
				s.ActiveCount, s.Omitted = remote.ActiveCount, remote.Omitted
				s.DirectSessions, s.RelaySessions = remote.DirectSessions, remote.RelaySessions
				s.LeaseOwner, s.LeaseExpires = remote.LeaseOwner, remote.LeaseExpires
				s.System = remote.System
			}
		}()
	}
	wg.Wait()
	// Keep the aggregate comfortably below the 8 MiB control-frame limit.
	remaining, recent := 4096, 512
	for i := range pool.Nodes {
		s := &pool.Nodes[i]
		if len(s.Active) > remaining {
			s.Omitted += len(s.Active) - remaining
			s.Active = s.Active[:remaining]
		}
		remaining -= len(s.Active)
		if len(s.Recent) > recent {
			s.Recent = s.Recent[:recent]
		}
		recent -= len(s.Recent)
	}
	pool.ObservedAt = time.Now().UTC()
	return pool, nil
}

func bounded(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "")
}
