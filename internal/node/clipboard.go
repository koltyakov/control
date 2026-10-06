package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
)

func (n *Node) serveClipboard(ctx context.Context, caller string, conn net.Conn, method string, params json.RawMessage) {
	var q clipboard.Request
	err := json.Unmarshal(params, &q)
	if err == nil {
		err = n.authorize(ctx, caller, method)
	}
	if err == nil && q.Protocol != clipboard.Protocol {
		err = errors.New("unsupported clipboard protocol")
	}
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	select {
	case n.clipboardSlots <- struct{}{}:
		defer func() { <-n.clipboardSlots }()
	default:
		_ = writeFrame(conn, model.Response{Error: "clipboard transfer limit reached"})
		return
	}
	ctx, release, err := n.enterWork(ctx)
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	defer release()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	defer stop()
	activity := n.beginActivity(ctx, "transfer", method, n.executionOwner(ctx, caller), caller)
	defer func() { activity.finish(err) }()
	if method == clipboard.OpenMethod {
		var value clipboard.Value
		value, err = n.readClipboard(ctx)
		var batch *clipboard.Batch
		if err == nil {
			batch, err = clipboard.Snapshot(value)
		}
		if err != nil {
			_ = writeFrame(conn, model.Response{Error: err.Error()})
			return
		}
		defer batch.Close()
		activity.progress(0, batch.Content.Result().Bytes+int64(32*len(batch.Content.Files)))
		if err = writeFrame(conn, model.Response{Result: model.JSON(clipboard.Ack{Protocol: clipboard.Protocol, Content: batch.Content})}); err != nil {
			return
		}
		var ready clipboard.Ready
		if err = readFrame(conn, &ready); err != nil {
			return
		}
		if !ready.Ready {
			err = errors.New("clipboard receiver did not accept paste")
			return
		}
		activity.phase("sending")
		err = batch.Send(ctx, activityWriter{writer: conn, counter: &activity.bytes})
		writeClipboardResult(conn, batch.Content.Result(), err)
		return
	}
	var root *os.Root
	err = q.Content.Validate()
	if err == nil && q.Content.Kind == "files" {
		if q.Dir == "" {
			q.Dir = "."
		}
		// Root.OpenRoot preserves the configured workspace boundary, including symlinks.
		root, err = n.root.OpenRoot(q.Dir)
		if err == nil {
			err = clipboard.CheckDestination(q.Content, root)
		}
	}
	if root != nil {
		defer func() { _ = root.Close() }()
	}
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	activity.progress(0, q.Content.Result().Bytes+int64(32*len(q.Content.Files)))
	if err = writeFrame(conn, model.Response{Result: model.JSON(clipboard.Ack{Protocol: clipboard.Protocol})}); err != nil {
		return
	}
	activity.phase("receiving")
	if q.Content.Kind == "text" {
		err = n.copyClipboard(ctx, q.Content.Text)
	} else {
		_, err = clipboard.Receive(ctx, q.Content, root, io.TeeReader(conn, activityWriter{writer: io.Discard, counter: &activity.bytes}))
	}
	writeClipboardResult(conn, q.Content.Result(), err)
}

func writeClipboardResult(conn net.Conn, result clipboard.Result, err error) {
	response := model.Response{Result: model.JSON(result)}
	if err != nil {
		response.Error = err.Error()
	}
	_ = writeFrame(conn, response)
}

// The local API relays only clipboard methods. It does not expose a generic streaming RPC proxy.
func (n *Node) clipboardAPI(w http.ResponseWriter, r *http.Request) {
	method, target := r.URL.Query().Get("method"), r.URL.Query().Get("target")
	if method != clipboard.OpenMethod && method != clipboard.PasteMethod {
		http.Error(w, "invalid clipboard method", http.StatusBadRequest)
		return
	}
	ctx, release, err := n.enterWork(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer release()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.CloseNow() }()
	ws.SetReadLimit(maxFrame)
	conn := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	// Bound the initial metadata read separately from the streamed file lifetime.
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	var q clipboard.Request
	if err = readFrame(conn, &q); err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if target == "" || target == n.Identity.ID || target == n.Config.Name {
		n.serveClipboard(ctx, n.Identity.ID, conn, method, model.JSON(q))
		return
	}
	peer, result, err := n.open(ctx, target, method, q)
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	defer func() { _ = peer.Close() }()
	if err = writeFrame(conn, model.Response{Result: result}); err != nil {
		return
	}
	Bridge(ctx, peer, conn)
}
