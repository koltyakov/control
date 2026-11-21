package transport

import (
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
)

func TestGatewayOnlyKindsRequireGatewaySender(t *testing.T) {
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{Identity: id, Node: model.Node{ID: id.ID, Name: "receiver", PublicKey: id.Public}}, nil)
	var controls []string
	p.SetControlHandler(func(kind string, _ []byte) { controls = append(controls, kind) })
	peer := "0b5621ebc737bda02bf2f4a4178d188d063774f75bdbcdf3d09b076668947cad"
	for _, kind := range []string{"update.command", "directory.changed"} {
		p.receive(&protocol.Packet{Kind: kind, From: peer})
	}
	if len(controls) != 0 || p.directoryGeneration != 0 {
		t.Fatal("peer-sent gateway-only packets reached handlers", controls, p.directoryGeneration)
	}
	for _, kind := range []string{"update.command", "directory.changed"} {
		p.receive(&protocol.Packet{Kind: kind, From: update.GatewaySender})
	}
	if len(controls) != 1 || p.directoryGeneration != 1 {
		t.Fatal("gateway packets were not handled", controls, p.directoryGeneration)
	}
	for _, kind := range []string{"proxy.request", "secrets.request", "gateway.drain", "gateway.resume", "gateway.ping", "machine.state", "wake.request", "startup.request", "update.anything"} {
		if !gatewayOnlyKind(kind) {
			t.Errorf("%s is not gateway-only", kind)
		}
	}
	for _, kind := range []string{"open", "answer", "data", "close", "ice", "lan.probe", "offline"} {
		if gatewayOnlyKind(kind) {
			t.Errorf("%s must remain peer-routable", kind)
		}
	}
}
