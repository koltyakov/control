package gateway

import "testing"

func TestGatewaySTUNValidation(t *testing.T) {
	for _, servers := range [][]string{nil, {"stun:localhost:3478"}, {"stuns:example.com:5349"}} {
		if err := validateSTUNServers(servers); err != nil {
			t.Fatal(servers, err)
		}
	}
	for _, servers := range [][]string{{"turn:example.com:3478"}, {"https://example.com"}, {"stun:"}, {""}, make([]string, 9)} {
		if err := validateSTUNServers(servers); err == nil {
			t.Fatal("accepted invalid STUN defaults", servers)
		}
	}
}
