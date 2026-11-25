package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/vaultstore"
	"github.com/koltyakov/control/internal/vaultwire"
	"golang.org/x/term"
)

const vaultUsage = `Control Vault

  control vault provision --frontend USER --gateway URL [--listen 127.0.0.1:7342]
                                  Provision under a dedicated broker OS account
  control vault serve             Run the broker under that account
  control vault enable [--receipt FILE]  Activate the broker's public record
  control vault migrate export|import    Offline cutover; use a private pipe
  control vault rollback export|restore|purge   Offline recovery (Unix)
  control vault status            Show this frontend's activation status
  control vault run [--ttl 24h] -- control COMMAND ...
                                  Approve and run one Control invocation
  control vault grant [--ttl 24h] CODE
                                  Approve the waiting connection from your chat
  control vault list              List pending requests and grants
  control vault revoke GRANT_ID   Stop a connection from issuing new commands
  control mcp --vault-process     Start a dedicated process-scoped stdio adapter
`

func vaultRoute(ctx context.Context, marker *vaultstore.Marker, global *flag.FlagSet, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		fmt.Print(vaultUsage)
		return nil
	}
	if args[0] == "version" {
		if len(args) > 1 && args[1] == "--json" {
			printJSON(buildinfo.Current())
		} else {
			fmt.Println("control", buildinfo.Version)
		}
		return nil
	}
	override := ""
	global.Visit(func(f *flag.Flag) {
		if f.Name == "api" || f.Name == "token" || f.Name == "config" {
			override = f.Name
		}
	})
	if override != "" {
		return fmt.Errorf("--%s cannot override Vault routing", override)
	}
	if args[0] == "mcp" {
		if len(args) != 2 || args[1] != "--vault-process" {
			return errors.New("vault requires an isolated MCP adapter: use the per-session host integration, or control mcp --vault-process to request access for one entire stdio connection")
		}
		peer := &lazyVaultPeer{marker: marker}
		defer peer.close()
		return vaultFrontend(ctx, []string{"mcp"}, peer)
	}
	return errors.New("vault is locked for this process; use control vault run -- control COMMAND, or an approved session adapter")
}

func vaultCLI(ctx context.Context, marker *vaultstore.Marker, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(vaultUsage)
		return nil
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return errors.New("usage: control vault status")
		}
		if marker == nil {
			fmt.Println("Vault is not enabled for this frontend.")
		} else {
			fmt.Println("Vault enabled; approval is per connection. Broker:", marker.Address)
		}
		return nil
	case "enable", "provision", "serve", "migrate", "rollback":
		return vaultSetup(ctx, args)
	}
	if marker == nil {
		return errors.New("vault is not enabled; see control vault help")
	}
	command := args[0]
	f := flag.NewFlagSet("vault "+command, flag.ContinueOnError)
	ttl := vaultwire.DefaultGrantTTL
	switch command {
	case "run", "grant":
		f.DurationVar(&ttl, "ttl", ttl, "absolute grant duration")
	case "list", "revoke":
	default:
		return fmt.Errorf("unknown vault command %q", command)
	}
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	args = f.Args()
	label := "Control Vault management"
	switch command {
	case "list":
		if len(args) != 0 {
			return errors.New("usage: control vault list")
		}
	case "run":
		if len(args) < 2 || args[0] != "control" {
			return errors.New("usage: control vault run [--ttl DURATION] -- control COMMAND [ARG...]")
		}
		args = args[1:]
		label = "Control CLI: " + vaultwire.SanitizeLabel(args[0])
		if args[0] == "mcp" {
			if len(args) != 2 || args[1] != "--vault-process" {
				return errors.New("explicitly name mcp --vault-process to authorize its entire stdio connection")
			}
			args = []string{"mcp"}
			label = "MCP stdio connection (all callers on this connection)"
		}
	default:
		if len(args) != 1 {
			return fmt.Errorf("vault %s requires one identifier", command)
		}
	}
	if ttl < time.Millisecond || ttl > vaultwire.MaxGrantTTL {
		return fmt.Errorf("vault TTL must be between 1ms and %s", vaultwire.MaxGrantTTL)
	}
	session, err := dialVault(ctx, marker, label)
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	code := ""
	if command == "run" {
		entry, requestErr := session.Request(ctx)
		if requestErr != nil {
			return requestErr
		}
		code = entry.Code
	}
	key, err := vaultApprovalKey(marker)
	if err != nil {
		return err
	}
	defer clear(key)
	switch command {
	case "list":
		entries, err := session.List(ctx, key)
		if err == nil {
			printJSON(entries)
		}
		return err
	case "revoke":
		return session.Revoke(ctx, key, args[0])
	case "grant":
		code = args[0]
	}
	// Display the exact broker record, not an identity claimed in chat. Reject
	// stale codes even if the local TTY is still showing an old approval prompt.
	entries, err := session.List(ctx, key)
	if err != nil {
		return err
	}
	var selected *vaultwire.Entry
	for i := range entries {
		if entries[i].Code != "" && entries[i].Code == code {
			selected = &entries[i]
			break
		}
	}
	if selected == nil || (command == "run" && selected.ID != session.ID()) {
		return errors.New("no pending request with that code; check it in the requesting session")
	}
	// A displayed request is not consent. The passphrase proof authorizes this
	// grant, while the confirmation lets the user reject a misleading label.
	if err := confirmVault(marker, code, selected.Label, ttl); err != nil {
		return err
	}
	entry, err := session.GrantEntry(ctx, key, *selected, ttl)
	clear(key) // The authorized command may run for hours; it needs no signing key.
	if err != nil {
		return err
	}
	if command == "grant" {
		printJSON(entry)
		return nil
	}
	return vaultFrontend(ctx, args, session)
}

// Listing a static MCP tool catalog must not create a pending request or a
// long-lived socket. Keep one physical connection once a tool actually runs.
// A lost connection or an uncertain call never triggers a reconnect or replay.
type lazyVaultPeer struct {
	mu      sync.Mutex
	marker  *vaultstore.Marker
	session *vaultwire.Session
	closed  bool
}

func (p *lazyVaultPeer) Do(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, vaultwire.ErrClosed
	}
	if p.session == nil {
		s, err := dialVault(ctx, p.marker, "MCP stdio connection (all callers on this connection)")
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		p.session = s
	}
	s := p.session
	p.mu.Unlock()
	return s.Do(ctx, op, args)
}

func (p *lazyVaultPeer) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	if p.session != nil {
		_ = p.session.Close()
	}
}

// Use a controlling terminal even when MCP stdio is attached to a pipe. Never
// accept a vault password from tool input, an environment variable, or argv.
func openVaultTTY() (*os.File, error) {
	path := "/dev/tty"
	if runtime.GOOS == "windows" {
		path = "CONIN$"
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, errors.New("vault approval requires a local controlling terminal")
	}
	if !term.IsTerminal(int(f.Fd())) {
		_ = f.Close()
		return nil, errors.New("vault approval requires a local controlling terminal")
	}
	return f, nil
}

func vaultTTYOutput(input *os.File) (*os.File, func(), error) {
	if runtime.GOOS != "windows" {
		return input, func() {}, nil
	}
	output, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return nil, nil, err
	}
	return output, func() { _ = output.Close() }, nil
}

func readVaultSecret(prompt string) ([]byte, error) {
	f, err := openVaultTTY()
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	output, done, err := vaultTTYOutput(f)
	if err != nil {
		return nil, err
	}
	defer done()
	if _, err := fmt.Fprint(output, prompt); err != nil {
		return nil, err
	}
	password, err := term.ReadPassword(int(f.Fd()))
	_, _ = fmt.Fprintln(output)
	if err != nil {
		clear(password)
		return nil, err
	}
	return password, nil
}

func confirmVault(marker *vaultstore.Marker, code, label string, ttl time.Duration) error {
	f, err := openVaultTTY()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	output, done, err := vaultTTYOutput(f)
	if err != nil {
		return err
	}
	defer done()
	if _, err := fmt.Fprintf(output, "Request %q, caller label %q (reported by caller).\nFleet gateway: %q. Allow this connection to request work across the fleet for %s? Fleet and worker access rules still apply.\nType yes to approve: ",
		vaultwire.SanitizeLabel(code), vaultwire.SanitizeLabel(label), marker.Gateway, ttl); err != nil {
		return err
	}
	var b [16]byte
	n, err := f.Read(b[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.TrimSpace(string(b[:n])) != "yes" {
		return errors.New("approval cancelled; no grant issued")
	}
	return nil
}

func vaultApprovalKey(marker *vaultstore.Marker) (ed25519.PrivateKey, error) {
	password, err := readVaultSecret("Vault passphrase: ")
	if err != nil {
		return nil, err
	}
	defer clear(password)
	return vaultwire.NewApprovalKey(password, marker.Salt)
}
