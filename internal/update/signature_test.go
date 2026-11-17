package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
)

func releaseManifestFixture(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(Manifest{Version: "v1", Assets: []Asset{{
		OS: "linux", Arch: "amd64", File: AssetName("linux", "amd64"),
		Size: 1, SHA256: strings.Repeat("a", 64),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func TestReleaseIdentityAndBytes(t *testing.T) {
	signer := testutil.NewReleaseSigner(t)
	data := releaseManifestFixture(t)
	proof := signer.Sign(t, "owner/repo", "v1", data)
	keys := ReleaseKeyring{"owner/repo": {signer.KeyID: signer.PublicKey(), "alias": signer.PublicKey()}, "other/repo": {signer.KeyID: signer.PublicKey()}}
	if err := keys.Verify(t.Context(), "owner/repo", "v1", data, proof); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, repo, tag string
		data, proof     []byte
	}{
		{"repository binding", "other/repo", "v1", data, proof},
		{"unknown repository", "unknown/repo", "v1", data, proof},
		{"tag binding", "owner/repo", "v2", data, proof},
		{"key ID binding", "owner/repo", "v1", data, bytes.Replace(proof, []byte(`"keyId":"fixture"`), []byte(`"keyId":"alias"`), 1)},
		{"whitespace", "owner/repo", "v1", append(bytes.Clone(data), ' '), proof},
		{"missing", "owner/repo", "v1", data, nil},
		{"legacy Sigstore", "owner/repo", "v1", data, []byte(`{"mediaType":"application/vnd.dev.sigstore.bundle.v0.3+json"}`)},
		{"oversized proof", "owner/repo", "v1", data, bytes.Repeat([]byte(" "), MaxReleaseSignatureSize+1)},
		{"oversized manifest", "owner/repo", "v1", bytes.Repeat([]byte(" "), MaxReleaseManifestSize+1), proof},
		{"invalid manifest", "owner/repo", "v1", []byte(`{}`), signer.Sign(t, "owner/repo", "v1", []byte(`{}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := keys.Verify(t.Context(), tc.repo, tc.tag, tc.data, tc.proof); err == nil {
				t.Fatal("accepted mismatched or invalid signature")
			}
		})
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := keys.Verify(cancelled, "owner/repo", "v1", data, proof); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
}

func TestConfiguredReleasePins(t *testing.T) {
	for repository, keys := range releaseKeys {
		for keyID, encoded := range keys {
			public, err := base64.StdEncoding.Strict().DecodeString(encoded)
			if err != nil || len(public) != ed25519.PublicKeySize {
				t.Fatalf("invalid public pin %s/%s", repository, keyID)
			}
			if _, err := ReleaseSignatureMessage(repository, "v1", keyID, releaseManifestFixture(t)); err != nil {
				t.Fatalf("invalid pin context: %v", err)
			}
		}
	}
}

func TestCheckReleaseKeys(t *testing.T) {
	const repository = "koltyakov/control"
	const keyID = "release-2026-11-17"
	valid, err := json.Marshal(releaseKeys[repository])
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, repo, id string
		data           []byte
		valid          bool
	}{
		{"matching", repository, keyID, valid, true},
		{"wrong repository", "other/repo", keyID, valid, false},
		{"missing ID", repository, "", valid, false},
		{"unknown ID", repository, "unknown", valid, false},
		{"missing keys", repository, keyID, nil, false},
		{"empty", repository, keyID, []byte("{}"), false},
		{"null", repository, keyID, []byte("null"), false},
		{"wrong type", repository, keyID, []byte("[]"), false},
		{"malformed", repository, keyID, []byte("{"), false},
		{"wrong key", repository, keyID, []byte(`{"release-2026-11-17":"wrong"}`), false},
		{"extra key", repository, keyID, bytes.Replace(valid, []byte("}"), []byte(`,"extra":"key"}`), 1), false},
		{"oversized", repository, keyID, bytes.Repeat([]byte(" "), MaxReleaseSignatureSize+1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckReleaseKeys(tc.repo, tc.id, tc.data); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}

func TestReleaseSignatureEnvelope(t *testing.T) {
	signer := testutil.NewReleaseSigner(t)
	data := releaseManifestFixture(t)
	proof := signer.Sign(t, "owner/repo", "v1", data)
	keys := ReleaseKeyring{"owner/repo": {signer.KeyID: signer.PublicKey()}}
	for name, malformed := range map[string][]byte{
		"empty": {}, "null": []byte("null"), "array": []byte("[]"),
		"truncated":           proof[:len(proof)-1],
		"trailing":            append(bytes.Clone(proof), []byte("{}")...),
		"duplicate":           bytes.Replace(proof, []byte(`"version":1`), []byte(`"version":1,"version":1`), 1),
		"unknown":             bytes.Replace(proof, []byte(`"version":1`), []byte(`"version":1,"publicKey":"attacker"`), 1),
		"case alias":          bytes.Replace(proof, []byte(`"keyId"`), []byte(`"KeyID"`), 1),
		"unsupported version": bytes.Replace(proof, []byte(`"version":1`), []byte(`"version":2`), 1),
		"missing signature":   []byte(`{"version":1,"keyId":"fixture"}`),
		"short signature":     []byte(`{"version":1,"keyId":"fixture","signature":"YQ=="}`),
		"bad base64":          []byte(`{"version":1,"keyId":"fixture","signature":"!!!"}`),
		"array signature":     []byte(`{"version":1,"keyId":"fixture","signature":[0,1,2]}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := keys.Verify(t.Context(), "owner/repo", "v1", data, malformed); err == nil {
				t.Fatal("accepted malformed signature")
			}
		})
	}
}

func TestReleaseKeyRotationAndInvalidPins(t *testing.T) {
	first, next := testutil.NewReleaseSigner(t), testutil.NewReleaseSigner(t)
	first.KeyID, next.KeyID = "first", "next"
	data := releaseManifestFixture(t)
	oldProof, newProof := first.Sign(t, "owner/repo", "v1", data), next.Sign(t, "owner/repo", "v1", data)
	for _, tc := range []struct {
		name      string
		keys      ReleaseKeyring
		old, next bool
	}{
		{"old", ReleaseKeyring{"owner/repo": {"first": first.PublicKey()}}, true, false},
		{"bridge", ReleaseKeyring{"owner/repo": {"first": first.PublicKey(), "next": next.PublicKey()}}, true, true},
		{"revoked", ReleaseKeyring{"owner/repo": {"next": next.PublicKey()}}, false, true},
		{"empty", ReleaseKeyring{}, false, false},
		{"bad encoding", ReleaseKeyring{"owner/repo": {"first": "!!!"}}, false, false},
		{"bad length", ReleaseKeyring{"owner/repo": {"first": "YQ=="}}, false, false},
		{"wrong key", ReleaseKeyring{"owner/repo": {"first": next.PublicKey()}}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, check := range []struct {
				proof []byte
				valid bool
			}{{oldProof, tc.old}, {newProof, tc.next}} {
				if err := tc.keys.Verify(t.Context(), "owner/repo", "v1", data, check.proof); (err == nil) != check.valid {
					t.Fatalf("valid=%v: %v", check.valid, err)
				}
			}
		})
	}
	// Ephemeral fixture keys must never be accepted by production pins.
	if err := VerifyReleaseManifest(t.Context(), "owner/repo", "v1", data, oldProof); err == nil {
		t.Fatal("test key trusted by production")
	}
}

func TestReleaseSignatureMessageRejectsInvalidContext(t *testing.T) {
	data := releaseManifestFixture(t)
	for _, tc := range []struct{ repo, tag, key string }{
		{"../repo", "v1", "key"}, {"owner/..", "v1", "key"}, {"owner/repo\x00", "v1", "key"},
		{"owner/repo", "../v1", "key"}, {"owner/repo", "v1", "key\x00"},
		{"owner/repo", "v1", ""}, {"owner/repo", "v2", "key"},
	} {
		if _, err := ReleaseSignatureMessage(tc.repo, tc.tag, tc.key, data); err == nil {
			t.Fatalf("accepted context %+v", tc)
		}
	}
}
