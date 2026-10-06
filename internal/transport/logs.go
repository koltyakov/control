package transport

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

const LogStreamVersion = "task-logs-v1"
const MaxLogChunk = 64 * 1024

func ValidateLogChunk(chunk model.TaskLogChunk, offset int64) error {
	if chunk.Error != "" {
		return errors.New(chunk.Error)
	}
	if len(chunk.Data) > MaxLogChunk || chunk.Offset != offset+int64(len(chunk.Data)) {
		return errors.New("invalid streamed log offset or chunk size")
	}
	return nil
}

// FollowTaskLogs uses one stream on upgraded receivers, or read-only polling on
// older ones. It never reconnects a broken stream or resubmits the task.
func (p *Peer) FollowTaskLogs(ctx context.Context, target, id string, offset int64, emit func(model.TaskLogChunk) error) error {
	if offset < 0 {
		return errors.New("invalid log offset")
	}
	conn, result, err := p.OpenRPC(ctx, target, "tasks.logs", map[string]any{"id": id, "offset": offset, "follow": true})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { abortStream(conn) })
	defer stop()
	var ack struct {
		Stream string `json:"stream"`
	}
	if err := json.Unmarshal(result, &ack); err != nil {
		return err
	}
	if ack.Stream == "" {
		return p.pollTaskLogs(ctx, target, id, offset, result, emit)
	}
	if ack.Stream != LogStreamVersion {
		return errors.New("unsupported task log stream version")
	}
	if err := emit(model.TaskLogChunk{Offset: offset}); err != nil {
		return err
	}
	for {
		var chunk model.TaskLogChunk
		if err := wire.ReadFrameLimit(conn, &chunk, 128*1024); err != nil {
			return err
		}
		if err := ValidateLogChunk(chunk, offset); err != nil {
			return err
		}
		if err := emit(chunk); err != nil {
			return err
		}
		offset = chunk.Offset
		if chunk.Terminal {
			return nil
		}
	}
}

func (p *Peer) pollTaskLogs(ctx context.Context, target, id string, offset int64, result json.RawMessage, emit func(model.TaskLogChunk) error) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var chunk struct {
			Text     string `json:"text"`
			Offset   int64  `json:"offset"`
			Terminal bool   `json:"terminal"`
		}
		if err := json.Unmarshal(result, &chunk); err != nil {
			return err
		}
		if chunk.Offset < offset || (chunk.Text != "" && chunk.Offset == offset) {
			return errors.New("invalid log offset")
		}
		terminal := chunk.Terminal && chunk.Text == ""
		if err := emit(model.TaskLogChunk{Data: []byte(chunk.Text), Offset: chunk.Offset, Terminal: terminal}); err != nil {
			return err
		}
		offset = chunk.Offset
		if terminal {
			return nil
		}
		if chunk.Text == "" {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
			}
		}
		conn, next, err := p.OpenRPC(ctx, target, "tasks.logs", map[string]any{"id": id, "offset": offset})
		if err != nil {
			return err
		}
		_ = conn.Close()
		result = next
	}
}
