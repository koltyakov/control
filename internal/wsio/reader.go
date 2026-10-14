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
// A Reader belongs to one read loop and must not be used concurrently.
type Reader struct {
	buffer bytes.Buffer
}

const maxRetainedBytes = 128 << 10

func (r *Reader) Read(ctx context.Context, ws *websocket.Conn) (websocket.MessageType, []byte, error) {
	kind, message, err := ws.Reader(ctx)
	if err != nil {
		return kind, nil, err
	}
	if r.buffer.Cap() > maxRetainedBytes {
		r.buffer = bytes.Buffer{}
	} else {
		r.buffer.Reset()
	}
	_, err = r.buffer.ReadFrom(message)
	if err != nil {
		return kind, nil, err
	}
	return kind, r.buffer.Bytes(), nil
}
