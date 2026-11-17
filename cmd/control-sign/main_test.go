package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/testutil"
	"github.com/koltyakov/control/internal/update"
)

func TestReleaseSigner(t *testing.T) {
	signer := testutil.NewReleaseSigner(t)
	key, err := x509.MarshalPKCS8PrivateKey(signer.Private)
	if err != nil {
		t.Fatal(err)
	}
	secret := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	keys := update.ReleaseKeyring{"owner/repo": {signer.KeyID: signer.PublicKey()}}
	for _, scenario := range []string{"valid", "missing-key", "invalid-key", "oversized-key", "extra-pem", "leading-garbage", "wrong-key-type", "wrong-key", "untrusted", "wrong-tag", "invalid-key-id", "manifest-as-output", "manifest-alias-output", "malformed-manifest", "oversized-manifest"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			manifestPath := filepath.Join(dir, "control-manifest.json")
			output := filepath.Join(dir, update.ReleaseSignatureName)
			manifest, err := json.Marshal(update.Manifest{Version: "v1", Assets: []update.Asset{{
				OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: 1, SHA256: strings.Repeat("a", 64),
			}}})
			if err != nil {
				t.Fatal(err)
			}
			input := bytes.Clone(secret)
			tag, keyID := "v1", signer.KeyID
			verify := keys.Verify
			switch scenario {
			case "missing-key":
				input = nil
			case "invalid-key":
				input = []byte("private-material-must-not-appear-in-errors")
			case "oversized-key":
				input = bytes.Repeat([]byte("k"), (16<<10)+1)
			case "extra-pem":
				input = append(input, secret...)
			case "leading-garbage":
				input = append([]byte("unrelated data\n"), input...)
			case "wrong-key-type":
				rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
				if err != nil {
					t.Fatal(err)
				}
				input = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
			case "untrusted":
				verify = update.VerifyReleaseManifest
			case "wrong-key":
				other := testutil.NewReleaseSigner(t)
				verify = update.ReleaseKeyring{"owner/repo": {signer.KeyID: other.PublicKey()}}.Verify
			case "wrong-tag":
				tag = "v2"
			case "invalid-key-id":
				keyID = "../key"
			case "manifest-as-output":
				output = manifestPath
			case "manifest-alias-output":
				output = dir + string(filepath.Separator) + "." + string(filepath.Separator) + "control-manifest.json"
			case "malformed-manifest":
				manifest = []byte(`{}`)
			case "oversized-manifest":
				manifest = bytes.Repeat([]byte(" "), update.MaxReleaseManifestSize+1)
			}
			if err := os.WriteFile(manifestPath, manifest, 0600); err != nil {
				t.Fatal(err)
			}
			args := []string{"--manifest", manifestPath, "--out", output, "--repository", "owner/repo", "--tag", tag, "--key-id", keyID}
			err = run(args, bytes.NewReader(input), verify)
			if scenario != "valid" {
				if err == nil {
					t.Fatal("invalid signing inputs accepted")
				}
				if strings.Contains(err.Error(), string(secret)) || strings.Contains(err.Error(), "private-material-must-not") {
					t.Fatal("private material in error")
				}
				if scenario != "manifest-as-output" && scenario != "manifest-alias-output" {
					if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("failed signing published output", err)
					}
				}
				after, err := os.ReadFile(manifestPath)
				if err != nil || !bytes.Equal(after, manifest) {
					t.Fatal("failed signing changed manifest", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			proof, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if err := keys.Verify(t.Context(), "owner/repo", "v1", manifest, proof); err != nil {
				t.Fatal(err)
			}
			if err := run(append(args, "--check"), noPrivateKeyReader{}, verify); err != nil {
				t.Fatal("check should not read a private key", err)
			}
			if err := os.WriteFile(manifestPath, append(manifest, ' '), 0600); err != nil {
				t.Fatal(err)
			}
			if err := run(append(args, "--check"), noPrivateKeyReader{}, verify); err == nil {
				t.Fatal("check accepted changed bytes")
			}
		})
	}
}

type noPrivateKeyReader struct{}

func (noPrivateKeyReader) Read([]byte) (int, error) {
	panic("verification must not read private keys")
}

func TestReleaseSignerRejectsUnexpectedArguments(t *testing.T) {
	err := run([]string{"unexpected"}, noPrivateKeyReader{}, func(context.Context, string, string, []byte, []byte) error { return nil })
	if err == nil {
		t.Fatal("unexpected arguments accepted")
	}
}

func TestReleaseSignerChecksPublicConfiguration(t *testing.T) {
	args := []string{"--check-keys", "--repository", "koltyakov/control", "--key-id", "release-2026-11-17"}
	const public = `{"release-2026-11-17":"JnsJqhMK87yVEeeKV3MuIJF7NIVy0OM87cTRcxAtWRA="}`
	for _, tc := range []struct {
		name, input string
		args        []string
		valid       bool
	}{
		{"matching", public, args, true},
		{"missing", "", args, false},
		{"mismatch", "{}", args, false},
		{"oversized", strings.Repeat(" ", update.MaxReleaseSignatureSize+1), args, false},
		{"conflicting checks", public, append(append([]string{}, args...), "--check"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			verify := func(context.Context, string, string, []byte, []byte) error {
				t.Fatal("public configuration check must not verify a manifest")
				return nil
			}
			if err := run(tc.args, strings.NewReader(tc.input), verify); (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
		})
	}
}
