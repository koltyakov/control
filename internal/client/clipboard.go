package client

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

type ClipboardSpec struct {
	Node    string `json:"node"`
	Reverse bool   `json:"reverse,omitempty"`
	Dir     string `json:"dir,omitempty"`
}

// PasteClipboard copies text to the destination clipboard or streams copied files
// into an existing directory. Reverse reads the remote clipboard instead.
func (c Client) PasteClipboard(ctx context.Context, spec ClipboardSpec) (clipboard.Result, error) {
	ctx, cancel := c.lifetimeContext(ctx)
	defer cancel()
	if spec.Node == "" {
		return clipboard.Result{}, errors.New("clipboard paste requires a target machine")
	}
	if spec.Reverse {
		return c.receiveClipboard(ctx, spec)
	}
	value, err := clipboard.Read(ctx)
	if err != nil {
		return clipboard.Result{}, err
	}
	batch, err := clipboard.Snapshot(value)
	if err != nil {
		return clipboard.Result{}, err
	}
	defer batch.Close()
	return c.sendClipboard(ctx, spec, batch)
}

func (c Client) openClipboard(ctx context.Context, target, method string, q clipboard.Request) (net.Conn, clipboard.Ack, error) {
	var ack clipboard.Ack
	peer, err := c.backend(ctx)
	if err != nil {
		return nil, ack, err
	}
	var conn net.Conn
	var data json.RawMessage
	if peer != nil {
		conn, data, err = peer.OpenRPC(ctx, target, method, q)
	} else {
		u := strings.TrimRight(c.URL, "/") + "/v1/clipboard?" + url.Values{"target": {target}, "method": {method}}.Encode()
		u = strings.Replace(strings.Replace(u, "https://", "wss://", 1), "http://", "ws://", 1)
		var ws *websocket.Conn
		ws, _, err = websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Token}}})
		if err == nil {
			ws.SetReadLimit(wire.MaxFrame)
			conn = websocket.NetConn(ctx, ws, websocket.MessageBinary)
			stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
			err = wire.WriteFrame(conn, q)
			if err == nil {
				data, err = clipboardResponse(conn)
			}
			stop()
		}
	}
	if err == nil {
		err = json.Unmarshal(data, &ack)
	}
	if err == nil && ack.Protocol != clipboard.Protocol {
		err = errors.New("receiver does not support clipboard-v1; upgrade the node")
	}
	if err != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, ack, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	return &contextConn{Conn: conn, stop: stop}, ack, nil
}

func clipboardResponse(conn net.Conn) (json.RawMessage, error) {
	var response model.Response
	if err := wire.ReadFrame(conn, &response); err != nil {
		return nil, err
	}
	if response.Error != "" {
		return nil, errors.New(response.Error)
	}
	return response.Result, nil
}

func (c Client) sendClipboard(ctx context.Context, spec ClipboardSpec, batch *clipboard.Batch) (clipboard.Result, error) {
	conn, _, err := c.openClipboard(ctx, spec.Node, clipboard.PasteMethod, clipboard.Request{Protocol: clipboard.Protocol, Content: batch.Content, Dir: spec.Dir})
	if err != nil {
		return clipboard.Result{}, err
	}
	defer func() { _ = conn.Close() }()
	if err = batch.Send(ctx, conn); err != nil {
		return clipboard.Result{}, err
	}
	data, err := clipboardResponse(conn)
	var result clipboard.Result
	if err == nil {
		err = json.Unmarshal(data, &result)
	}
	return result, err
}

func (c Client) receiveClipboard(ctx context.Context, spec ClipboardSpec) (clipboard.Result, error) {
	conn, ack, err := c.openClipboard(ctx, spec.Node, clipboard.OpenMethod, clipboard.Request{Protocol: clipboard.Protocol})
	if err != nil {
		return clipboard.Result{}, err
	}
	defer func() { _ = conn.Close() }()
	if err = ack.Content.Validate(); err != nil {
		return clipboard.Result{}, err
	}
	var root *os.Root
	if ack.Content.Kind == "files" {
		if spec.Dir == "" {
			spec.Dir = "."
		}
		root, err = os.OpenRoot(spec.Dir)
		if err != nil {
			return clipboard.Result{}, err
		}
		defer func() { _ = root.Close() }()
		if err = clipboard.CheckDestination(ack.Content, root); err != nil {
			return clipboard.Result{}, err
		}
	}
	if err = wire.WriteFrame(conn, clipboard.Ready{Ready: true}); err != nil {
		return clipboard.Result{}, err
	}
	if root != nil {
		if _, err = clipboard.Receive(ctx, ack.Content, root, conn); err != nil {
			return clipboard.Result{}, err
		}
	}
	if _, err = clipboardResponse(conn); err != nil {
		return clipboard.Result{}, err
	}
	if ack.Content.Kind == "text" {
		if err = clipboard.Copy(ctx, ack.Content.Text); err != nil {
			return clipboard.Result{}, err
		}
	}
	return ack.Content.Result(), nil
}
