package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/vaultops"
	"github.com/koltyakov/control/internal/vaultstore"
	"github.com/koltyakov/control/internal/vaultwire"
)

func currentVault() (*vaultstore.Marker, error) {
	marker, err := vaultstore.LoadMarker(installation.Home())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read Vault activation; refusing credential fallback: %w", err)
	}
	layout, err := vaultstore.DefaultLayout()
	if err != nil || marker.Source != layout.MarkerPath() {
		return nil, errors.New("vault activation does not reference the installed broker record")
	}
	return &marker, nil
}

func dialVault(ctx context.Context, marker *vaultstore.Marker, label string) (*vaultwire.Session, error) {
	return vaultwire.Dial(ctx, marker.Address, marker.Pin, label)
}

func vaultSetup(ctx context.Context, args []string) error {
	layout, err := vaultstore.DefaultLayout()
	if err != nil {
		return err
	}
	switch args[0] {
	case "enable":
		f := flag.NewFlagSet("vault enable", flag.ContinueOnError)
		receiptPath := f.String("receipt", "", "signed receipt from an offline migration")
		revoked := f.Bool("revoked-old-keys", false, "confirm revocation of superseded credentials listed on the receipt")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		if f.NArg() > 1 || (f.NArg() == 1 && f.Arg(0) != layout.MarkerPath()) {
			return errors.New("vault enable uses the broker-owned record at " + layout.MarkerPath())
		}
		marker, err := readVaultMarker(layout.MarkerPath())
		if err != nil {
			return err
		}
		// Verify the public record actually reaches the pinned broker before
		// making any change to local routing. Never exchange a fleet credential.
		session, err := dialVault(ctx, &marker, "activation check")
		if err != nil {
			return err
		}
		_ = session.Close()
		opts := vaultstore.ActivateOptions{Source: layout.MarkerPath(), RevocationConfirmed: *revoked}
		if *receiptPath != "" {
			var receipt vaultstore.Receipt
			if err := readVaultPublicJSON(*receiptPath, 1<<20, &receipt); err != nil {
				return err
			}
			opts.Receipt = &receipt
		}
		if _, err := vaultstore.Activate(installation.Home(), opts); err != nil {
			return err
		}
		fmt.Println("Vault enabled. Use control vault run for CLI commands or connect a dedicated session adapter.")
		return nil
	case "provision":
		return provisionVault(ctx, layout, args[1:])
	case "serve":
		if len(args) != 1 {
			return errors.New("run control vault serve under the dedicated broker account")
		}
		return runProvisionedVault(ctx, layout)
	case "migrate":
		return migrateVault(layout, args[1:])
	case "rollback":
		return rollbackVault(layout, args[1:])
	}
	return errors.New("unknown Vault setup command")
}

func rollbackVault(layout vaultstore.Layout, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: control vault rollback export|restore|purge")
	}
	switch args[0] {
	case "export":
		info, err := os.Stdout.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("rollback releases protected credentials; pipe the broker's export directly to the frontend's vault rollback restore")
		}
		bundle, err := vaultstore.Rollback(layout)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(bundle)
	case "restore":
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("rollback restore requires a pipe from the broker account's rollback export")
		}
		return vaultstore.RestoreLegacy(installation.Home(), os.Stdin)
	case "purge":
		return vaultstore.Purge(layout)
	default:
		return errors.New("expected vault rollback export, restore, or purge")
	}
}

func readVaultMarker(path string) (vaultstore.Marker, error) {
	var m vaultstore.Marker
	err := readVaultPublicJSON(path, 8192, &m)
	if err == nil {
		err = m.Validate()
	}
	return m, err
}

func readVaultPublicJSON(path string, limit int64, dst any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil || !fi.Mode().IsRegular() {
		return errors.New("public Vault record must be a regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return errors.New("unreadable or oversized public Vault record")
	}
	return json.Unmarshal(b, dst)
}

func provisionVault(ctx context.Context, layout vaultstore.Layout, args []string) error {
	f := flag.NewFlagSet("vault provision", flag.ContinueOnError)
	address := f.String("listen", "127.0.0.1:7342", "fixed broker loopback address")
	base := f.String("gateway", "", "trusted fleet gateway URL")
	frontend := f.String("frontend", "", "everyday OS account that will use this broker")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *base == "" || *frontend == "" {
		return errors.New("usage: control vault provision --frontend USER --gateway URL [--listen LOOPBACK:PORT]")
	}
	gateway, err := enrollment.GatewayURL(*base)
	if err != nil {
		return err
	}
	target, err := url.Parse(gateway)
	if err != nil || (target.Scheme != "https" && !net.ParseIP(target.Hostname()).IsLoopback()) {
		return errors.New("use an HTTPS gateway, or a numeric loopback address for a local gateway")
	}
	if err := vaultstore.CheckProvision(layout, *frontend); err != nil {
		return err
	}
	secret, err := readVaultSecret("Gateway account key: ")
	if err != nil {
		return err
	}
	remote := client.Admin{URL: gateway, Key: strings.TrimSpace(string(secret))}
	clear(secret)
	role, err := remote.AuthRole(ctx)
	if err != nil {
		return err
	}
	if role != "user" && role != "orchestrator" {
		return errors.New("provision Vault with a fleet account or orchestrator key, not an operator or worker key")
	}
	pass, err := readVaultSecret("New Vault approval passphrase: ")
	if err != nil {
		return err
	}
	defer clear(pass)
	if len(pass) < 12 {
		return errors.New("use a Vault passphrase of at least 12 bytes")
	}
	confirmation, err := readVaultSecret("Confirm Vault approval passphrase: ")
	if err != nil {
		return err
	}
	match := bytes.Equal(pass, confirmation)
	clear(confirmation)
	if !match {
		return errors.New("passphrases do not match")
	}
	salt, err := vaultwire.NewApprovalSalt()
	if err != nil {
		return err
	}
	key, err := vaultwire.NewApprovalKey(pass, salt)
	if err != nil {
		return err
	}
	defer clear(key)
	marker, err := vaultstore.Provision(layout, vaultstore.ProvisionRequest{Address: *address, Frontend: *frontend,
		Account: client.StandaloneConfig{Gateway: remote, AccountRouting: true}, ApprovalKey: key.Public().(ed25519.PublicKey), Salt: salt})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(marker)
}

func runProvisionedVault(ctx context.Context, layout vaultstore.Layout) error {
	p, err := vaultstore.OpenServer(layout)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	listener, err := net.Listen("tcp", p.Address())
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	return serveVault(ctx, listener, p.TLSConfig().Certificates[0], p.ApprovalKey(), p.Account(), p.TunnelRoot())
}

func migrateVault(layout vaultstore.Layout, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: control vault migrate export | (as broker account) control vault migrate import")
	}
	switch args[0] {
	case "export":
		info, err := os.Stdout.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("the migration bundle contains credentials; pipe it directly into control vault migrate import")
		}
		bundle, err := vaultstore.ExportMigration(installation.Home())
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(bundle)
	case "import":
		info, err := os.Stdin.Stat()
		if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			return errors.New("vault migrate import requires a pipe from the frontend export")
		}
		receipt, err := vaultstore.Import(layout, os.Stdin)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(receipt)
	default:
		return errors.New("expected vault migrate export or import")
	}
}

func serveVault(ctx context.Context, listener net.Listener, cert tls.Certificate, key ed25519.PublicKey, cfg client.StandaloneConfig, tunnelDir string) error {
	r, err := vaultops.Open(ctx, cfg, tunnelDir)
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	return vaultwire.Serve(ctx, listener, cert, key, func(connection context.Context) vaultwire.Handler {
		return vaultOperationSession{r.NewSession(connection)}
	})
}

type vaultOperationSession struct{ *vaultops.Handler }

func (s vaultOperationSession) Close() { _ = s.Handler.Close() }
