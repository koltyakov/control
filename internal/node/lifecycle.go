package node

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var ErrUnregistered = errors.New("machine was unregistered by its fleet owner")

// Unregistered reports whether the node durably received identity retirement.
func (n *Node) Unregistered() bool { return n.currentMachineState().Unregistered }

func (n *Node) loadMachineState() error {
	err := store.Read(filepath.Join(n.Config.DataDir, "machine-state.json"), &n.machineState)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	n.work.SetDisabled(n.machineState.Disabled || n.machineState.Unregistered)
	return nil
}

func (n *Node) syncMachineState(ctx context.Context) error {
	current := n.currentMachineState()
	if current.Unregistered {
		return ErrUnregistered
	}
	state, supported, err := n.Peer.MachineState(ctx, current.Revision)
	if err != nil || !supported {
		return err
	}
	if err := n.applyMachineState(state); err != nil {
		return err
	}
	if state != current {
		// Acknowledge only after the policy is durable and admission has changed.
		_, _, err = n.Peer.MachineState(ctx, state.Revision)
	}
	return err
}

func (n *Node) applyMachineState(state model.MachineState) error {
	n.lifecycleMu.Lock()
	defer n.lifecycleMu.Unlock()
	if state.Revision < n.machineState.Revision {
		return errors.New("gateway machine state revision moved backwards")
	}
	if state != n.machineState {
		// Block admission before persistence. Failure cannot accidentally enable work.
		if state.Disabled || state.Unregistered {
			n.work.SetDisabled(true)
		}
		if err := store.Write(filepath.Join(n.Config.DataDir, "machine-state.json"), state); err != nil {
			return err
		}
		n.machineState = state
		n.work.SetDisabled(state.Disabled || state.Unregistered)
		if state.Unregistered {
			return ErrUnregistered
		}
	}
	return nil
}

func (n *Node) runMachineState(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if err := n.syncMachineState(ctx); errors.Is(err, ErrUnregistered) {
			n.cancel()
			if n.shutdown != nil {
				n.shutdown()
			}
			return
		} else if err != nil && ctx.Err() == nil {
			slog.Debug("machine state refresh", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (n *Node) currentMachineState() model.MachineState {
	n.lifecycleMu.Lock()
	defer n.lifecycleMu.Unlock()
	return n.machineState
}
