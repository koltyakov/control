package update

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestIndependentPublicationOnlyBlocksUnresolvedCommitsAndSwitch(t *testing.T) {
	r, err := OpenRepository(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("fixture")
	sum := sha256.Sum256(data)
	a := Asset{OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"), Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	if err := r.Upload(bytes.NewReader(data), a); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: "v1", Assets: []Asset{a}}
	d, err := r.Publish(m, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SetPhase(d.ID, "installing", nil); err != nil {
		t.Fatal(err)
	}
	m.Version = "v2"
	d, err = r.Publish(m, "test")
	if err != nil {
		t.Fatal("merely pending nodes blocked publication", err)
	}
	if err := r.NodeInstall(d.ID, "worker", true); err != nil {
		t.Fatal(err)
	}
	m.Version = "v3"
	if _, err := r.Publish(m, "test"); err == nil || !strings.Contains(err.Error(), "unresolved node update commits: worker") {
		t.Fatal("unresolved node commit was superseded or hidden behind a generic install error", err)
	}
	if err := r.NodeInstall(d.ID, "worker", false); err != nil {
		t.Fatal(err)
	}
	if err := r.SetGatewayState(d.ID, "switching"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(m, "test"); err == nil || !strings.Contains(err.Error(), "gateway update switch is in progress") {
		t.Fatal("gateway switch was superseded or hidden behind a generic install error", err)
	}
	if err := r.SetGatewayState(d.ID, "error"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Publish(m, "test"); err != nil {
		t.Fatal("failed switch prevented safe publication", err)
	}
}
