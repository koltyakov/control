package testutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"testing"
)

// ReleaseSigner uses an ephemeral key, never a production release pin.
type ReleaseSigner struct {
	Private ed25519.PrivateKey
	KeyID   string
}

func NewReleaseSigner(t *testing.T) ReleaseSigner {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return ReleaseSigner{Private: private, KeyID: "fixture"}
}

func (s ReleaseSigner) PublicKey() string {
	return base64.StdEncoding.EncodeToString(s.Private.Public().(ed25519.PublicKey))
}

// Sign implements the documented protocol independently of the verifier.
func (s ReleaseSigner) Sign(t *testing.T, repository, tag string, manifest []byte) []byte {
	t.Helper()
	message := append([]byte("control-release-manifest-v1\x00"+repository+"\x00"+tag+"\x00"+s.KeyID+"\x00"), manifest...)
	proof, err := json.Marshal(struct {
		Version   int    `json:"version"`
		KeyID     string `json:"keyId"`
		Signature []byte `json:"signature"`
	}{1, s.KeyID, ed25519.Sign(s.Private, message)})
	if err != nil {
		t.Fatal(err)
	}
	return proof
}
