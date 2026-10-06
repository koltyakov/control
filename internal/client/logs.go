package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// FollowLogs resumes from a byte offset and ends at task completion or caller
// cancellation. Cancelling a log follower never cancels the durable task.
func (c Client) FollowLogs(ctx context.Context, target, id string, offset int64, dest io.Writer) error {
	ctx, cancel := c.lifetimeContext(ctx)
	defer cancel()
	if offset < 0 {
		return errors.New("invalid log offset")
	}
	peer, err := c.backend(ctx)
	if err != nil {
		return err
	}
	emit := func(chunk model.TaskLogChunk) error {
		n, err := dest.Write(chunk.Data)
		if err == nil && n != len(chunk.Data) {
			err = io.ErrShortWrite
		}
		return err
	}
	if peer != nil {
		return peer.FollowTaskLogs(ctx, target, id, offset, emit)
	}
	u := strings.TrimRight(c.URL, "/") + "/v1/logs?" + url.Values{"target": {target}, "id": {id}, "offset": {strconv.FormatInt(offset, 10)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return c.followLogsPoll(ctx, target, id, offset, dest)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("log stream: %s", resp.Status)
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 128*1024)
	for scanner.Scan() {
		var chunk model.TaskLogChunk
		if err := json.Unmarshal(scanner.Bytes(), &chunk); err != nil {
			return err
		}
		if err := transport.ValidateLogChunk(chunk, offset); err != nil {
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
	if err := scanner.Err(); err != nil {
		return err
	}
	return io.ErrUnexpectedEOF
}

func (c Client) followLogsPoll(ctx context.Context, target, id string, offset int64, dest io.Writer) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var logs struct {
			Text     string `json:"text"`
			Offset   int64  `json:"offset"`
			Terminal bool   `json:"terminal"`
		}
		if err := c.Call(ctx, target, "tasks.logs", map[string]any{"id": id, "offset": offset}, &logs); err != nil {
			return err
		}
		if logs.Offset < offset || (logs.Text != "" && logs.Offset == offset) {
			return errors.New("log offset moved backwards")
		}
		if _, err := io.WriteString(dest, logs.Text); err != nil {
			return err
		}
		offset = logs.Offset
		// A terminal log response can still have unread chunks of bounded logs.
		if logs.Terminal && logs.Text == "" {
			return nil
		}
		if logs.Text != "" {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
