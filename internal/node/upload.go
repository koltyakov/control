package node

import (
	"context"
	"encoding/json"
	"net"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
)

func (n *Node) serveUpload(ctx context.Context, caller string, conn net.Conn, params json.RawMessage) {
	var q upload.Request
	var err error
	if len(params) > upload.MetadataLimit {
		err = upload.Error("invalid_upload", "upload metadata exceeds 16 KiB")
	} else {
		err = json.Unmarshal(params, &q)
	}
	if err == nil {
		err = n.authorize(ctx, caller, upload.OpenMethod)
	}
	if err != nil {
		_ = writeFrame(conn, model.ErrorResponse(err))
		return
	}
	ctx, release, err := n.enterWork(ctx)
	if err != nil {
		_ = writeFrame(conn, model.ErrorResponse(err))
		return
	}
	defer release()
	releaseInvocation, err := n.reserveLeaseSensitiveStream(ctx)
	if err != nil {
		_ = writeFrame(conn, model.ErrorResponse(err))
		return
	}
	defer releaseInvocation()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()); _ = conn.Close() })
	defer stop()
	activity := n.beginActivity(ctx, "transfer", upload.OpenMethod, n.executionOwner(ctx, caller), caller)
	activity.progress(0, q.Size)
	activity.phase("receiving")
	err = n.uploads.Serve(ctx, n.executionOwner(ctx, caller), conn, q, func() error { return n.authorize(ctx, caller, upload.OpenMethod) }, func(bytes int64) { activity.progress(bytes, q.Size) })
	activity.finish(err)
}
