package enrollment

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

func TestResolveInvitationName(t *testing.T) {
	for _, hostname := range []string{"DESKTOP-123", "render.local", "machine_01"} {
		name, err := (Invitation{AutoName: true}).ResolveName(hostname)
		if err != nil || name != hostname {
			t.Fatalf("hostname %q changed to %q: %v", hostname, name, err)
		}
	}
	if name, err := (Invitation{Name: "chosen-name"}).ResolveName("actual-host"); err != nil || name != "chosen-name" {
		t.Fatalf("explicit name changed: %q, %v", name, err)
	}
	for _, hostname := range []string{"", "-host", "a b", "主机", "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijkl"} {
		if _, err := (Invitation{AutoName: true}).ResolveName(hostname); err == nil {
			t.Fatalf("accepted invalid hostname %q", hostname)
		}
	}
}

func TestEnrollmentProofPreservesLegacyAndSignsTargetName(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proof := Redemption{PublicKey: pub, CredentialHash: Hash("credential"), Arch: "amd64"}
	legacy, err := json.Marshal(struct {
		Purpose, Ticket, Credential string
		PublicKey                   []byte
		Arch                        string `json:"arch,omitempty"`
	}{"control-enrollment-v1", Hash("ticket"), proof.CredentialHash, pub, proof.Arch})
	if err != nil || string(Message("ticket", proof)) != string(legacy) {
		t.Fatal("named invitation proof changed", err)
	}
	proof.Name = "TARGET-PC"
	proof.Signature = ed25519.Sign(key, Message("ticket", proof))
	if !ed25519.Verify(pub, Message("ticket", proof), proof.Signature) {
		t.Fatal("target-name proof did not verify")
	}
	proof.Name = "another-host"
	if ed25519.Verify(pub, Message("ticket", proof), proof.Signature) {
		t.Fatal("signature did not bind the target hostname")
	}
}
