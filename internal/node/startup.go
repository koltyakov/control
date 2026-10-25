package node

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// ConfigureStartup is installed by the executable before Start. The handler may
// prepare only the running profile's migration, without scripts, paths, or UAC.
func (n *Node) ConfigureStartup(prepare func(context.Context, string) error) {
	var mu sync.Mutex
	attempted := false
	n.Peer.SetStartupHandler(func(data []byte) {
		var q model.StartupRequest
		if json.Unmarshal(data, &q) != nil || q.ID == "" || len(q.ID) > 64 || (q.Mode != "system" && q.Mode != "hybrid") {
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
		defer cancel()
		mu.Lock()
		defer mu.Unlock()
		reply := model.StartupReply{ID: q.ID, Status: "manual"}
		var err error
		switch {
		case attempted:
			err = errors.New("a startup change was already admitted; inspect the machine before running recovery commands")
		case n.Config.InstructionOnly || !accountMayCall(n.Config.Allow, "service.startup"):
			err = errors.New("this node does not permit gateway startup changes")
		case n.shutdown == nil || prepare == nil:
			err = errors.New("this node has no installed startup migration handler")
		default:
			if err = n.reserveStartupChange(); err == nil {
				err = prepare(ctx, q.Mode)
				if err != nil {
					n.maintenanceMu.Lock()
					n.mu.Lock()
					n.startupChanging = false
					n.mu.Unlock()
					n.work.Resume()
					n.maintenanceMu.Unlock()
				} else {
					attempted = true
					reply.Status = "accepted"
				}
			}
		}
		if err != nil {
			reply.Reason = err.Error()
			if len(reply.Reason) > 500 {
				reply.Reason = reply.Reason[:500]
			}
		}
		_ = n.Peer.SendControl(ctx, "startup.result", model.JSON(reply))
		if reply.Status == "accepted" {
			// The prepared helper waits for shutdown and the supervisor's lock.
			// A lost reply never causes another migration attempt.
			n.shutdown()
		}
	})
}

func (n *Node) reserveStartupChange() error {
	n.maintenanceMu.Lock()
	defer n.maintenanceMu.Unlock()
	if !n.work.ReserveIfIdle() {
		return errors.New("the machine is busy, disabled, starting, or reserved for an update; wait until idle")
	}
	if n.updateBusy() {
		n.work.Resume()
		return errors.New("the machine has a lease or pending work; wait until idle")
	}
	n.mu.Lock()
	n.startupChanging = true
	n.mu.Unlock()
	return nil
}
