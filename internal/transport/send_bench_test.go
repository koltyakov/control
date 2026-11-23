package transport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/protocol"
)

// BenchmarkRelaySend measures the sender side of one relayed stream: packetConn
// writes split into packets, marshaled, queued, and written to the gateway
// socket by the peer's writer.
func BenchmarkRelaySend(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(-1)
		buffer := make([]byte, 128<<10)
		for {
			_, reader, err := ws.Reader(r.Context())
			if err != nil {
				return
			}
			if _, err = io.CopyBuffer(io.Discard, reader, buffer); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = ws.CloseNow() }()
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(ctx)
	defer p.cancel()
	p.gatewayWS.Store(ws)
	done := make(chan struct{})
	go func() { defer close(done); p.writeGateway() }()
	defer func() { p.cancel(); <-done }()
	l := &link{id: "session", remote: "peer", mode: "relay"}
	l.conn = newConn(p.ctx, func(ctx context.Context, data []byte) error {
		return p.send(ctx, &protocol.Packet{Kind: "data", To: "peer", Session: l.id, Data: data})
	}, nil)
	l.conn.packetSize = largePacketSize
	payload := make([]byte, 256<<10)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := l.conn.Write(payload); err != nil {
			b.Fatal(err)
		}
	}
}
