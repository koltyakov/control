// control-sign signs release manifests at build time. It is not part of control.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdin, update.VerifyReleaseManifest); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, verify func(context.Context, string, string, []byte, []byte) error) error {
	flags := flag.NewFlagSet("control-sign", flag.ContinueOnError)
	manifestPath := flags.String("manifest", "dist/control-manifest.json", "final manifest to sign")
	output := flags.String("out", "dist/"+update.ReleaseSignatureName, "detached signature file")
	repository := flags.String("repository", "", "exact GitHub owner/repository")
	tag := flags.String("tag", "", "exact release tag")
	keyID := flags.String("key-id", "", "pinned key ID")
	check := flags.Bool("check", false, "verify an existing signature without reading a private key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected signing arguments")
	}
	manifest, err := readFile(*manifestPath, update.MaxReleaseManifestSize)
	if err != nil {
		return err
	}
	if *check {
		proof, err := readFile(*output, update.MaxReleaseSignatureSize)
		if err != nil {
			return err
		}
		return verify(context.Background(), *repository, *tag, manifest, proof)
	}
	sourcePath, err := filepath.Abs(*manifestPath)
	if err != nil {
		return err
	}
	outputPath, err := filepath.Abs(*output)
	if err != nil {
		return err
	}
	sourceInfo, sourceErr := os.Stat(sourcePath)
	outputInfo, outputErr := os.Stat(outputPath)
	if sourcePath == outputPath || (sourceErr == nil && outputErr == nil && os.SameFile(sourceInfo, outputInfo)) {
		return errors.New("signature output must differ from the manifest")
	}
	message, err := update.ReleaseSignatureMessage(*repository, *tag, *keyID, manifest)
	if err != nil {
		return err
	}
	// Read the secret only after validating public inputs. Never log input or
	// parser errors that might include private material.
	secret, err := readLimited(stdin, 16<<10)
	if err != nil {
		return errors.New("read signing key: missing, unreadable, or oversized input")
	}
	block, rest := pem.Decode(secret)
	if block == nil || block.Type != "PRIVATE KEY" || len(block.Headers) != 0 ||
		!strings.HasPrefix(strings.TrimSpace(string(secret)), "-----BEGIN PRIVATE KEY-----") || strings.TrimSpace(string(rest)) != "" {
		return errors.New("signing key must be one unencrypted PKCS#8 PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return errors.New("invalid PKCS#8 signing key")
	}
	private, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(private) != ed25519.PrivateKeySize {
		return errors.New("signing key must be Ed25519")
	}
	proof := update.ReleaseSignature{Version: 1, KeyID: *keyID, Signature: ed25519.Sign(private, message)}
	data, err := json.Marshal(proof)
	if err != nil {
		return err
	}
	// A wrong/missing pin must stop publication, not produce an unusable release.
	if err := verify(context.Background(), *repository, *tag, manifest, data); err != nil {
		return err
	}
	return store.Bytes(*output, append(data, '\n'), 0600)
}

func readFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return readLimited(file, limit)
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("empty or oversized signing input")
	}
	return data, nil
}
