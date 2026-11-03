package buildinfo

import "testing"

func TestGatewayCompatibilityIsExplicitAndBounded(t *testing.T) {
	for _, test := range []struct {
		info  Info
		epoch int
		want  bool
	}{
		{Info{}, 0, false}, {Info{}, 1, false},
		{Info{NodeGatewayMin: 0, NodeGatewayMax: 1}, 0, true},
		{Info{NodeGatewayMin: 0, NodeGatewayMax: 1}, 1, true},
		{Info{NodeGatewayMin: 0, NodeGatewayMax: 1}, 2, false},
		{Info{NodeGatewayMin: 2, NodeGatewayMax: 3}, 1, false},
		{Info{NodeGatewayMin: -1, NodeGatewayMax: 1}, 0, false},
		{Info{NodeGatewayMin: 2, NodeGatewayMax: 1}, 1, false},
	} {
		if got := test.info.SupportsGateway(test.epoch); got != test.want {
			t.Fatal(test, got)
		}
	}
}
