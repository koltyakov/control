package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/testutil"
)

// A proxied node streams managed binaries from its gateway through the proxy
// and never tries GitHub, which its proxy does not reach.
func TestProxiedManagedDownloadUsesGatewayOnly(t *testing.T) {
	data := bytes.Repeat([]byte("managed binary "), 64<<10)
	sum := sha256.Sum256(data)
	asset := Asset{OS: "linux", Arch: "amd64", File: "control_linux_amd64", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	server, pool := testutil.TLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(data)
	}), "gateway.invalid")
	t.Cleanup(gatewayroute.SetRootCAsForTesting(pool))
	_, port, _ := net.SplitHostPort(server.Listener.Addr().String())
	target := "gateway.invalid:" + port
	proxy := testutil.NewConnectProxy(t, map[string]string{target: server.Listener.Addr().String()})
	route, err := gatewayroute.New(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "control")
	if err = DownloadPreferred(t.Context(), route, "koltyakov/control", "v1.2.3", "https://"+target+"/v1/updates/blobs/"+asset.SHA256, "node-token", path, asset); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatal("downloaded binary differs")
	}
	for _, seen := range proxy.Targets() {
		if seen != target {
			t.Fatalf("proxied download contacted %s", seen)
		}
	}
	// A corrupt stream is rejected by the pinned digest.
	corrupt := asset
	corrupt.SHA256 = hex.EncodeToString(make([]byte, 32))
	if err = DownloadPreferred(t.Context(), route, "", "v1", "https://"+target+"/blob", "node-token", filepath.Join(t.TempDir(), "bad"), corrupt); err == nil {
		t.Fatal("corrupt binary accepted")
	}
}
