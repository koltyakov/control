package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// Capture the wakeup channel before reading the log to avoid losing an append
// or task completion between the file read and the subscription.
func (n *Node) readTaskLog(owner, id string, offset int64) (model.TaskLogChunk, <-chan struct{}, error) {
	n.mu.Lock()
	task, err := n.ownedTaskLocked(owner, id)
	if err != nil {
		n.mu.Unlock()
		return model.TaskLogChunk{}, nil, err
	}
	terminal := task.Terminal()
	var changed <-chan struct{}
	if notifier := n.notifiers[id]; notifier != nil {
		changed = notifier.next()
	}
	n.mu.Unlock()
	chunk := model.TaskLogChunk{Offset: offset}
	if !terminal && changed == nil {
		// A task without a notifier is settling; poll its state once more.
		closed := make(chan struct{})
		close(closed)
		changed = closed
	}
	f, err := os.Open(n.logPath(id))
	if errors.Is(err, os.ErrNotExist) {
		chunk.Terminal = terminal
		return chunk, changed, nil
	}
	if err != nil {
		return chunk, changed, err
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return chunk, changed, err
	}
	chunk.Data, err = io.ReadAll(io.LimitReader(f, transport.MaxLogChunk))
	chunk.Offset += int64(len(chunk.Data))
	// Send a terminal record only after all retained bytes have been drained.
	chunk.Terminal = terminal && len(chunk.Data) == 0
	return chunk, changed, err
}

func (n *Node) localTaskLogs(ctx context.Context, owner, id string, offset int64, emit func(model.TaskLogChunk) error) error {
	if offset < 0 {
		return errors.New("invalid log offset")
	}
	first := true
	for {
		chunk, changed, err := n.readTaskLog(owner, id, offset)
		if err != nil {
			return err
		}
		if first || len(chunk.Data) > 0 || chunk.Terminal {
			if err := emit(chunk); err != nil {
				return err
			}
			first = false
		}
		offset = chunk.Offset
		if chunk.Terminal {
			return nil
		}
		if len(chunk.Data) != 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

func (n *Node) serveTaskLogs(ctx context.Context, caller string, conn net.Conn, args json.RawMessage) {
	var q struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
	}
	err := json.Unmarshal(args, &q)
	if err == nil {
		err = n.authorize(ctx, caller, "tasks.logs")
	}
	if err == nil && q.Offset < 0 {
		err = errors.New("invalid log offset")
	}
	if err == nil {
		_, _, err = n.readTaskLog(n.executionOwner(ctx, caller), q.ID, q.Offset)
	}
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	if err := writeFrame(conn, model.Response{Result: model.JSON(map[string]any{"stream": transport.LogStreamVersion, "offset": q.Offset})}); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readDone := make(chan struct{})
	go func() { defer close(readDone); var b [1]byte; _, _ = conn.Read(b[:]); cancel() }()
	defer func() { _ = conn.SetReadDeadline(time.Now()); <-readDone }()
	err = n.localTaskLogs(ctx, n.executionOwner(ctx, caller), q.ID, q.Offset, func(chunk model.TaskLogChunk) error { return writeFrame(conn, chunk) })
	if err != nil && ctx.Err() == nil {
		_ = writeFrame(conn, model.TaskLogChunk{Error: err.Error(), Offset: q.Offset})
	}
}

func (n *Node) FollowTaskLogs(ctx context.Context, target, id string, offset int64, emit func(model.TaskLogChunk) error) error {
	n.mu.Lock()
	if n.closed || n.ctx == nil || n.ctx.Err() != nil {
		n.mu.Unlock()
		return errors.New("node is not running")
	}
	n.wg.Add(1)
	n.mu.Unlock()
	defer n.wg.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(n.ctx, cancel)
	defer stop()
	if target == "" || target == n.Config.Name || target == n.Identity.ID {
		return n.localTaskLogs(ctx, n.Identity.ID, id, offset, emit)
	}
	return n.Peer.FollowTaskLogs(ctx, target, id, offset, emit)
}
