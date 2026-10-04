package transport

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
)

func TestRegistrationSoftwareKeepsLegacySignatureEncoding(t *testing.T) {
	p := New(Config{}, nil)
	p.SetSoftware(buildinfo.Info{Version: "dev-test", ReleaseRepo: "owner/fork", BuildTime: "2026-10-03T12:00:00Z", OS: "linux", Arch: "amd64", SHA256: "checksum"})
	wire, err := json.Marshal(p.cfg.Node.Software)
	if err != nil {
		t.Fatal(err)
	}
	// Older gateways decode and re-encode this original field set when
	// verifying the registration signature during rolling updates.
	var legacy struct {
		Version string `json:"version"`
		OS      string `json:"os"`
		Arch    string `json:"arch"`
		SHA256  string `json:"sha256,omitempty"`
	}
	if err := json.Unmarshal(wire, &legacy); err != nil {
		t.Fatal(err)
	}
	reencoded, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wire, reencoded) {
		t.Fatalf("registration breaks older gateway signatures: %s != %s", wire, reencoded)
	}
}
