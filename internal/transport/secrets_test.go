package transport

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func TestSecretsHandlerIsGatewayOnlyAndBounded(t *testing.T) {
	t.Parallel()
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(t.Context())
	t.Cleanup(func() { _ = p.Close() })
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	p.SetSecretsHandler(nil, func([]byte) {
		calls.Add(1)
		entered <- struct{}{}
		<-p.ctx.Done()
	})
	p.receive(&protocol.Packet{Kind: "secrets.request", From: "another-worker", Data: []byte("forged")})
	p.receive(&protocol.Packet{Kind: "secrets.request", From: update.GatewaySender, Data: make([]byte, (12<<10)+1)})
	p.receive(&protocol.Packet{Kind: "secrets.request", From: update.GatewaySender, Data: []byte("accepted")})
	select {
	case <-entered:
	case <-t.Context().Done():
		t.Fatal("handler did not start")
	}
	p.receive(&protocol.Packet{Kind: "secrets.request", From: update.GatewaySender, Data: []byte("excess")})
	if err := p.Close(); err != nil || calls.Load() != 1 {
		t.Fatal("unauthorized or excess handler admitted, or shutdown did not join it")
	}
}
