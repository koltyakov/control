package update

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strings"
)

const ReleaseSignatureName = "control-manifest.sig"
const MaxReleaseManifestSize = 64 << 10
const MaxReleaseSignatureSize = 4 << 10

// ReleaseSignature is a versioned detached Ed25519 signature, not a key source.
type ReleaseSignature struct {
	Version   int    `json:"version"`
	KeyID     string `json:"keyId"`
	Signature []byte `json:"signature"`
}

// ReleaseKeyring maps exact repositories and key IDs to base64 Ed25519 public keys.
// Production uses only the reviewed pins in release_keys.go, never downloaded keys.
type ReleaseKeyring map[string]map[string]string

var releaseKeyIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// CheckReleaseKeys checks public CI configuration against reviewed source pins.
// Configuration never replaces the keys trusted by a running executable.
func CheckReleaseKeys(repository, keyID string, data []byte) error {
	if len(data) > MaxReleaseSignatureSize {
		return errors.New("release public keyring exceeds limit")
	}
	var configured map[string]string
	if err := json.Unmarshal(data, &configured); err != nil {
		return errors.New("release public keyring must be a JSON object")
	}
	pinned := releaseKeys[repository]
	if len(pinned) == 0 || !releaseKeyIDPattern.MatchString(keyID) || pinned[keyID] == "" ||
		!maps.Equal(configured, pinned) {
		return errors.New("release public keyring or active key ID differs from compiled pins")
	}
	return nil
}

// VerifyReleaseManifest authenticates the exact manifest bytes with a pinned key.
func VerifyReleaseManifest(ctx context.Context, repository, tag string, manifest, proof []byte) error {
	return releaseKeys.Verify(ctx, repository, tag, manifest, proof)
}

// Verify permits isolated keyrings for build tooling and tests without changing
// the process-wide production pins.
func (keys ReleaseKeyring) Verify(ctx context.Context, repository, tag string, manifest, proof []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(manifest) > MaxReleaseManifestSize || len(proof) > MaxReleaseSignatureSize {
		return errors.New("release signature input exceeds limit")
	}
	sig, err := decodeReleaseSignature(proof)
	if err != nil {
		return fmt.Errorf("release signature: %w", err)
	}
	if sig.Version != 1 || !releaseKeyIDPattern.MatchString(sig.KeyID) || len(sig.Signature) != ed25519.SignatureSize {
		return errors.New("release signature: invalid version, key ID, or signature length")
	}
	encoded, ok := keys[repository][sig.KeyID]
	if !ok {
		return errors.New("release signature: repository or key ID is not pinned")
	}
	public, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(public) != ed25519.PublicKeySize {
		return errors.New("release signature: invalid pinned public key")
	}
	message, err := ReleaseSignatureMessage(repository, tag, sig.KeyID, manifest)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(public), message, sig.Signature) {
		return errors.New("release signature: verification failed")
	}
	return ctx.Err()
}

// ReleaseSignatureMessage defines the v1 signing protocol for build-time tooling.
// NUL-delimited context fields exclude NUL themselves. Manifest bytes are last
// and are never reserialized, so even whitespace changes invalidate a signature.
func ReleaseSignatureMessage(repository, tag, keyID string, manifest []byte) ([]byte, error) {
	parts := strings.Split(repository, "/")
	if len(repository) > 256 || !repositoryPattern.MatchString(repository) || len(parts) != 2 ||
		parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." ||
		!versionPattern.MatchString(tag) || !releaseKeyIDPattern.MatchString(keyID) {
		return nil, errors.New("release signature: invalid repository, tag, or key ID")
	}
	if len(manifest) > MaxReleaseManifestSize {
		return nil, errors.New("release manifest exceeds limit")
	}
	var metadata Manifest
	if err := json.Unmarshal(manifest, &metadata); err != nil {
		return nil, errors.New("release signature: invalid manifest JSON")
	}
	if err := metadata.Validate(); err != nil {
		return nil, fmt.Errorf("release signature: %w", err)
	}
	if metadata.Version != tag {
		return nil, errors.New("release tag and manifest version differ")
	}
	return append([]byte("control-release-manifest-v1\x00"+repository+"\x00"+tag+"\x00"+keyID+"\x00"), manifest...), nil
}

func decodeReleaseSignature(data []byte) (ReleaseSignature, error) {
	var sig ReleaseSignature
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return sig, errors.New("expected signature object")
	}
	seen := map[string]bool{}
	for decoder.More() {
		token, err = decoder.Token()
		if err != nil {
			return sig, errors.New("invalid signature field")
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return sig, errors.New("duplicate or invalid signature field")
		}
		seen[name] = true
		switch name {
		case "version":
			err = decoder.Decode(&sig.Version)
		case "keyId":
			err = decoder.Decode(&sig.KeyID)
		case "signature":
			var encoded string
			if err = decoder.Decode(&encoded); err == nil {
				sig.Signature, err = base64.StdEncoding.Strict().DecodeString(encoded)
			}
		default:
			return sig, errors.New("unknown signature field")
		}
		if err != nil {
			return sig, errors.New("invalid signature field value")
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return sig, errors.New("incomplete signature object")
	}
	var trailing json.RawMessage
	if err = decoder.Decode(&trailing); err != io.EOF {
		return sig, errors.New("trailing signature data")
	}
	return sig, nil
}
