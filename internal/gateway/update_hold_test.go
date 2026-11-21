package gateway

import (
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/update"
)

func TestGatewayHoldsRestartWhileNodesDownload(t *testing.T) {
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: 1, SHA256: "aa"}
	d := &update.Deployment{ID: "next", Manifest: update.Manifest{Version: "v2", Assets: []update.Asset{asset}}}
	old := model.Node{ID: "worker", Online: true, Software: buildinfo.Info{Version: "v1", OS: "linux", Arch: "amd64", SHA256: "bb"}}
	current := old
	current.Software.Version, current.Software.SHA256 = "v2", "aa"
	staged := time.Now()
	peer := func(n model.Node, id, state string) rolloutPeer {
		return rolloutPeer{node: n, fresh: true, status: update.Status{ID: id, State: state}}
	}
	for _, tc := range []struct {
		name  string
		peers []rolloutPeer
		at    time.Time
		hold  bool
	}{
		{"offer not yet acknowledged", []rolloutPeer{peer(old, "previous", "applied")}, staged.Add(time.Second), true},
		{"acknowledgement window elapsed", []rolloutPeer{peer(old, "previous", "applied")}, staged.Add(gatewayOfferSettle), false},
		{"node downloading", []rolloutPeer{peer(old, "next", "downloading")}, staged.Add(time.Minute), true},
		{"node staged", []rolloutPeer{peer(old, "next", "staged")}, staged.Add(time.Second), false},
		{"node already current", []rolloutPeer{peer(current, "previous", "applied")}, staged.Add(time.Second), false},
		{"stale peer", []rolloutPeer{{node: old, status: update.Status{ID: "next", State: "downloading"}}}, staged.Add(time.Second), false},
		{"hold bound reached", []rolloutPeer{peer(old, "next", "downloading")}, staged.Add(gatewayDownloadHold), false},
	} {
		if got := gatewayHoldForDownloads(d, tc.peers, staged, tc.at); got != tc.hold {
			t.Errorf("%s: hold = %v, want %v", tc.name, got, tc.hold)
		}
	}
}
