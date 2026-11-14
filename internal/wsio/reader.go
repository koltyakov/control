// Package wsio provides reusable message storage for serial WebSocket readers.
package wsio

import (
	"bytes"
	"context"

	"github.com/coder/websocket"
)

// Reader retains storage for ordinary relay packets, but not large signaling
// messages. The connection's SetReadLimit remains the message-size authority.
// Returned bytes are borrowed until the next Read; decode or copy before then.
// Oversized storage is detached as soon as it is returned, so it lives only as
// long as the caller's reference and never while waiting for the next message.
// A Reader belongs to one read loop and must not be used concurrently.
type Reader struct {
	buffer bytes.Buffer
}

const maxRetainedBytes = 128 << 10

func (r *Reader) Read(ctx context.Context, ws *websocket.Conn) (websocket.MessageType, []byte, error) {
	r.buffer.Reset()
	kind, message, err := ws.Reader(ctx)
	if err != nil {
		return kind, nil, err
	}
	_, err = r.buffer.ReadFrom(message)
	data := r.buffer.Bytes()
	if r.buffer.Cap() > maxRetainedBytes {
		r.buffer = bytes.Buffer{}
	}
	if err != nil {
		return kind, nil, err
	}
	return kind, data, nil
}
