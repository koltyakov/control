package node

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

type workContextKey struct{}

func observational(method string) bool {
	switch method {
	case "nodes.list", "nodes.select", "node.describe", "system.info", "capabilities.list", "activities.list", "activities.pool", "tasks.get", "tasks.list", "tasks.logs", "leases.get", "artifacts.list":
		return true
	default:
		return false
	}
}

func requiresAdmission(method string) bool {
	return !observational(method) && method != "tasks.cancel" && method != "leases.release" && method != "leases.renew"
}

func (n *Node) enterWork(ctx context.Context) (context.Context, func(), error) {
	if owner, _ := ctx.Value(workContextKey{}).(*Node); owner == n {
		return ctx, func() {}, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(n.ctx, cancel)
	release, err := n.work.Enter(ctx)
	if err != nil {
		stop()
		cancel()
		return ctx, nil, err
	}
	return context.WithValue(ctx, workContextKey{}, n), func() { release(); stop(); cancel() }, nil
}

func (n *Node) updateBusy() bool {
	active, _ := n.work.State()
	if active > 0 {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy())
}

func (n *Node) pauseForUpdate() bool {
	if !n.work.PauseIfIdle() {
		return false
	}
	if n.updateBusy() {
		n.work.Resume()
		return false
	}
	return true
}

// ConfigureUpdates is called by the executable before Start. Libraries/tests
// cannot accidentally replace their own test runner through an update message.
func (n *Node) ConfigureUpdates(info buildinfo.Info, apply func(string) error) error {
	n.Peer.SetSoftware(info)
	a, err := update.NewAgent(update.AgentOptions{
		Gateway: n.Config.Gateway, Token: n.Config.Token, Dir: n.Config.DataDir, Software: info,
		Busy: n.updateBusy, Pause: n.pauseForUpdate, Resume: n.work.Resume, Apply: apply,
		Report: func(ctx context.Context, status update.Status) error {
			return n.Peer.SendControl(ctx, "update.status", model.JSON(status))
		},
	})
	if err != nil {
		return err
	}
	n.updater = a
	n.Peer.SetControlHandler(a.Receive)
	return nil
}

type workConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *workConn) Close() error { err := c.Conn.Close(); c.once.Do(c.release); return err }
