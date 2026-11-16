package transport

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func TestProxyManagementIsGatewayOnlyBoundedAndJoined(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx, p.cancel = context.WithCancel(t.Context())
	t.Cleanup(func() { _ = p.Close() })
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	p.SetProxyManagementHandler(func([]byte) {
		calls.Add(1)
		entered <- struct{}{}
		<-p.ctx.Done()
	})
	p.receive(&protocol.Packet{Kind: "proxy.request", From: "another-worker", Data: []byte("forged")})
	p.receive(&protocol.Packet{Kind: "proxy.request", From: update.GatewaySender, Data: make([]byte, 8193)})
	p.receive(&protocol.Packet{Kind: "proxy.request", From: update.GatewaySender, Data: []byte("accepted")})
	<-entered
	p.receive(&protocol.Packet{Kind: "proxy.request", From: update.GatewaySender, Data: []byte("excess")})
	if err := p.Close(); err != nil || calls.Load() != 1 {
		t.Fatal("unauthorized/excess request admitted or shutdown did not join handler")
	}
}
