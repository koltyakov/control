// Package client talks to a local node. CLI and MCP share this implementation.
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
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

type Client struct {
	URL   string
	Token string
}

func (c Client) request(ctx context.Context, path string, value any) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.URL, "/")+path, bytes.NewReader(model.JSON(value)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

func (c Client) Call(ctx context.Context, target, method string, params any, result any) error {
	resp, err := c.request(ctx, "/v1/call", node.APICall{Target: target, Method: method, Params: model.JSON(params)})
	if err != nil {
		return fmt.Errorf("local node API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("local node API: %s", resp.Status)
	}
	var response model.Response
	if err = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&response); err != nil {
		return err
	}
	if response.Error != "" {
		return errors.New(response.Error)
	}
	if result == nil {
		return nil
	}
	return json.Unmarshal(response.Result, result)
}

func (c Client) Wait(ctx context.Context, target, id string) (model.Task, error) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var task model.Task
		if err := c.Call(ctx, target, "tasks.get", map[string]any{"id": id}, &task); err != nil {
			return task, err
		}
		if task.Terminal() {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c Client) Download(ctx context.Context, a model.Artifact, offset int64, w io.Writer) error {
	resp, err := c.request(ctx, "/v1/download", map[string]any{"artifact": a, "offset": offset})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	_, err = io.CopyN(w, resp.Body, a.Size-offset)
	return err
}

func (c Client) Tunnel(ctx context.Context, target, address string) (net.Conn, error) {
	u := strings.TrimRight(c.URL, "/") + "/v1/tunnel?" + url.Values{"target": {target}, "address": {address}}.Encode()
	u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
	ws, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
	if err != nil {
		return nil, err
	}
	return websocket.NetConn(ctx, ws, websocket.MessageBinary), nil
}
