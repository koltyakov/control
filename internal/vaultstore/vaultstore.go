// Package vaultstore provisions and opens the protected account-client profile
// served by a dedicated, non-administrative OS account (the vault broker), and
// the nonsecret activation marker used by the everyday (frontend) account.
//
// The security boundary is the operating system's account separation, not
// encryption. The broker account owns a private directory that the frontend
// account cannot read or write, and every protected path is opened component by
// component from the filesystem root without following symbolic links, so an
// ancestor cannot be substituted. Callers never supply a trusted owner, a
// state directory, or an environment override; trust is derived from the
// running process credentials and from kernel-reported file ownership.
//
// Supported platforms are Linux, macOS and Windows. Every other platform fails
// closed with ErrUnsupported. On Windows the root is fixed in ProgramData, must
// be owned by an administrator principal, and legacy plaintext migration is
// refused; see trust_windows.go.
//
// The package performs no network I/O, never creates OS accounts, and never
// requests elevation. An administrator creates the broker account and its root
// directory; everything else runs as either the broker or the frontend.
package vaultstore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
)

var (
	// ErrUnsupported reports a platform or setup without a verifiable OS
	// account boundary. Nothing is read or written.
	ErrUnsupported = errors.New("vault: protected profiles are unsupported on this platform")
	// ErrBusy reports a live vault server or tunnel service that must be
	// stopped first. Nothing was changed.
	ErrBusy = errors.New("vault: profile is in use")
	// ErrExists reports an already provisioned profile. It is never replaced.
	ErrExists = errors.New("vault: profile already exists")
	// ErrRolledBack reports a profile disabled by Rollback.
	ErrRolledBack = errors.New("vault: profile was rolled back")
	// ErrActivationMissing reports a trusted vault marker for this account
	// without a local activation. Callers must not fall back to plaintext mode.
	ErrActivationMissing = errors.New("vault: this account has a provisioned vault but no activation marker; run vault activation instead of a plaintext login")
	// ErrUnverified reports a vault marker or activation that exists but
	// could not be verified. It never matches os.ErrNotExist, so callers must
	// not fall back to plaintext mode.
	ErrUnverified = errors.New("vault: marker present but unverified; refusing plaintext mode")
	// ErrAmbientCredentials reports plaintext account, owner, update or tunnel
	// credentials, or credential environment variables, visible to the frontend.
	ErrAmbientCredentials = errors.New("vault: plaintext credentials remain in the everyday profile")
	// ErrIncomplete reports a migration receipt that does not cover the
	// frontend's current plaintext state. Nothing was deleted.
	ErrIncomplete = errors.New("vault: migration receipt does not cover the current plaintext state")
)

const (
	markerVersion  = 1
	profileVersion = 1
	bundleVersion  = 1
	receiptVersion = 1

	// MarkerName is the public marker in the layout root and, once activated,
	// the frontend copy in the Control home directory.
	MarkerName = "vault.json"

	stateActive     = "active"
	stateRolledBack = "rolled-back"

	receiptContext = "control-vault-receipt-v1\x00"

	minSaltSize = 16
	maxSaltSize = 64
	maxFileSize = 1 << 20
	maxBundle   = 8 << 20
)

// Bundle kinds.
const (
	BundleMigration = "migration"
	BundleRollback  = "rollback"
)

// Item kinds name the plaintext frontend files a migration covers.
const (
	ItemAccount        = "account"         // admin.json: account API key
	ItemUpdate         = "update"          // update-admin.json: superuser key
	ItemOwner          = "owner"           // clients/<scope>/identity.key: task owner
	ItemTunnelService  = "tunnel-service"  // tunnels/<scope>/service.json: tunnel login
	ItemTunnels        = "tunnels"         // tunnels/<scope>/tunnels.json: definitions
	ItemTunnelEndpoint = "tunnel-endpoint" // tunnels/<scope>/endpoint.json: management token
)

// Receipt dispositions.
const (
	// Imported items are durably stored in the protected profile.
	Imported = "imported"
	// Superseded items are credentials replaced by the fresh broker login.
	// They remain valid at the gateway until revoked.
	Superseded = "superseded"
	// Discarded items are ephemeral and need no protected copy.
	Discarded = "discarded"
)

// Principal is a kernel account identity. UID is a decimal uid on Unix and a
// SID string on Windows.
type Principal struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

// Marker is the nonsecret description of a provisioned vault. It carries no
// credential, approval key or private key. Pin is the lowercase hex SHA-256 of
// Certificate (DER), which is the broker's self-signed TLS certificate.
type Marker struct {
	Version     int       `json:"version"`
	Address     string    `json:"address"`
	Pin         string    `json:"pin"`
	Certificate []byte    `json:"certificate"`
	Salt        []byte    `json:"salt"`
	Broker      Principal `json:"broker"`
	Frontend    Principal `json:"frontend"`
	Gateway     string    `json:"gateway"`
	Created     time.Time `json:"created"`
	// Source is the trusted marker path. It is set only in the frontend copy
	// and is re-verified on every LoadMarker.
	Source string `json:"source,omitempty"`
	// Pending marks a frontend copy written before plaintext removal. It
	// keeps an interrupted activation from falling back to plaintext mode.
	Pending bool `json:"pending,omitempty"`
}

// Layout locates a vault. Root must be an existing directory owned by the
// broker account, not writable by any other account, reachable from "/"
// without symbolic links. It holds the public marker and the 0700 private
// profile directory. Prefer DefaultLayout; a manual Root gets the same checks.
type Layout struct {
	Root string
}

// ProvisionRequest describes a clean vault. It deliberately has no state
// directory: protected state always lives in the broker's private directory.
type ProvisionRequest struct {
	// Address is the loopback HOST:PORT the vault server listens on.
	Address string
	// Frontend is the everyday OS account name allowed to activate the vault.
	Frontend string
	// Account is a fresh account login entered directly into the broker. Its
	// StateDir must be empty; AccountRouting is forced on.
	Account client.StandaloneConfig
	// Update is an optional fresh superuser login for managed updates.
	Update *client.Admin
	// ApprovalKey is the public half of vaultwire.NewApprovalKey(pass, Salt).
	// No passphrase or private approval key is stored.
	ApprovalKey ed25519.PublicKey
	Salt        []byte
}

// BundleItem is one plaintext file moved between frontend and broker. Path is
// relative to the Control home with forward slashes.
type BundleItem struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Data   []byte `json:"data"`
}

// Bundle carries plaintext over a pipe between the two accounts. It must never
// be written to disk or passed in argv.
type Bundle struct {
	Version  int          `json:"version"`
	Kind     string       `json:"kind"`
	Frontend Principal    `json:"frontend"`
	Items    []BundleItem `json:"items"`
}

// ReceiptItem records what the broker did with one bundle item.
type ReceiptItem struct {
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Digest      string `json:"digest"`
	Disposition string `json:"disposition"`
}

// Receipt is signed by the broker's TLS key after the protected copies were
// synced and re-read. It contains no secret.
type Receipt struct {
	Version  int           `json:"version"`
	Pin      string        `json:"pin"`
	Frontend Principal     `json:"frontend"`
	Items    []ReceiptItem `json:"items"`
	// RevokeRequired describes superseded credentials that remain valid at
	// the gateway. Activation requires the caller to confirm revocation.
	RevokeRequired []string  `json:"revokeRequired,omitempty"`
	Issued         time.Time `json:"issued"`
	Signature      []byte    `json:"signature,omitempty"`
}

// ActivateOptions controls frontend activation.
type ActivateOptions struct {
	// Source is the trusted marker path; empty means DefaultLayout. A manual
	// path is verified exactly like the default and is never trusted by name.
	Source string
	// Receipt is required when plaintext credentials exist in the home.
	Receipt *Receipt
	// RevocationConfirmed acknowledges that every Receipt.RevokeRequired
	// credential was revoked at the gateway. It is an operator assertion, not
	// a verification.
	RevocationConfirmed bool
	// NodeConfigs adds node configurations selected by the caller, such as
	// --config. HOME/node.json and CONTROL_CONFIG are always checked.
	NodeConfigs []string
}

// AmbientError lists plaintext credentials visible to the frontend.
type AmbientError struct {
	Paths []string
	Env   []string
}

func (e *AmbientError) Error() string {
	var parts []string
	if len(e.Paths) > 0 {
		parts = append(parts, "files "+strings.Join(e.Paths, ", "))
	}
	if len(e.Env) > 0 {
		parts = append(parts, "environment "+strings.Join(e.Env, ", "))
	}
	return ErrAmbientCredentials.Error() + ": " + strings.Join(parts, "; ")
}

func (e *AmbientError) Unwrap() error { return ErrAmbientCredentials }

// ambientEnv names environment variables that carry account, superuser, or
// node/local API credentials.
var ambientEnv = []string{"CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN"}

// nodeConfigCredential reports whether this process can read a node
// configuration holding a fleet token, which also authenticates its local API.
// An unreadable (permission-denied) file is not ambient; an accessible file
// that cannot be parsed is reported, failing closed.
func nodeConfigCredential(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return false, err
	}
	var cfg struct {
		Token string `json:"token"`
	}
	if len(b) > maxFileSize || json.Unmarshal(b, &cfg) != nil {
		return true, nil
	}
	return cfg.Token != "", nil
}

// nodeConfigPaths lists the node configurations a CLI in this home could use.
// The environment only adds paths to check; it never removes one.
func nodeConfigPaths(home string, extra []string) []string {
	paths := []string{filepath.Join(home, "node.json")}
	if env := os.Getenv("CONTROL_CONFIG"); env != "" {
		paths = append(paths, env)
	}
	for _, p := range append(paths, extra...) {
		if p != "" && !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	return paths
}

// checkAmbientSources reports credential environment variables and accessible
// node tokens, plus any plaintext paths already found by the caller.
func checkAmbientSources(home string, paths, nodeConfigs []string) error {
	for _, cfg := range nodeConfigPaths(home, nodeConfigs) {
		found, err := nodeConfigCredential(cfg)
		if err != nil {
			return fmt.Errorf("cannot check node configuration %s: %w", cfg, err)
		}
		if found {
			paths = append(paths, cfg+" (node token)")
		}
	}
	var env []string
	for _, name := range ambientEnv {
		if os.Getenv(name) != "" {
			env = append(env, name)
		}
	}
	if len(paths) == 0 && len(env) == 0 {
		return nil
	}
	return &AmbientError{Paths: paths, Env: env}
}

// Pin returns the lowercase hex SHA-256 of a DER certificate.
func Pin(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validateAddress(address string) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("vault address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("vault address must be a loopback IP literal")
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return errors.New("vault address needs a fixed port between 1 and 65535")
	}
	return nil
}

func validateGateway(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return errors.New("vault account needs a gateway URL")
	}
	return nil
}

func validateSalt(salt []byte) error {
	if len(salt) < minSaltSize || len(salt) > maxSaltSize {
		return fmt.Errorf("approval salt must have %d..%d bytes", minSaltSize, maxSaltSize)
	}
	return nil
}

func validatePrincipal(p Principal) error {
	if !validAccountID(p.UID) || p.Name == "" {
		return errors.New("invalid account principal")
	}
	return nil
}

// validAccountID reports whether id names an account on this platform: a
// decimal uid on Unix, or on Windows an ordinary user SID (see userSID).
func validAccountID(id string) bool {
	if runtime.GOOS == "windows" {
		return userSID(id)
	}
	_, err := strconv.ParseUint(id, 10, 32)
	return err == nil
}

// superuserID reports the platform superuser: uid 0 on Unix. Windows
// built-in and service accounts already fail userSID.
func superuserID(id string) bool {
	return runtime.GOOS != "windows" && id == "0"
}

// userSID accepts a canonical SID for a local or domain user account
// (S-1-5-21-a-b-c-RID with RID >= 1000, which excludes built-in accounts such
// as Administrator and Guest) or a Microsoft Entra ID user (S-1-12-1-a-b-c-d).
// Well-known, service, virtual and group SIDs are refused. It is pure syntax;
// callers compare it with kernel-reported SIDs.
func userSID(id string) bool {
	parts := strings.Split(id, "-")
	if len(parts) != 8 || parts[0] != "S" || parts[1] != "1" {
		return false
	}
	for _, part := range parts[2:] {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != part {
			return false
		}
	}
	switch {
	case parts[2] == "5" && parts[3] == "21":
		rid, _ := strconv.ParseUint(parts[7], 10, 32)
		return rid >= 1000
	case parts[2] == "12" && parts[3] == "1":
		return true
	}
	return false
}

// Validate checks a marker's internal consistency. It does not establish that
// the marker is trusted; LoadMarker and Activate verify its source.
func (m Marker) Validate() error {
	if m.Version != markerVersion {
		return errors.New("unsupported vault marker version; upgrade this Control binary")
	}
	if err := validateAddress(m.Address); err != nil {
		return err
	}
	if err := validateSalt(m.Salt); err != nil {
		return err
	}
	if err := validateGateway(m.Gateway); err != nil {
		return err
	}
	if err := validatePrincipal(m.Broker); err != nil {
		return fmt.Errorf("vault broker: %w", err)
	}
	if err := validatePrincipal(m.Frontend); err != nil {
		return fmt.Errorf("vault frontend: %w", err)
	}
	if m.Broker.UID == m.Frontend.UID || superuserID(m.Broker.UID) || superuserID(m.Frontend.UID) {
		return errors.New("vault broker and frontend must be distinct non-root accounts")
	}
	if _, err := certificateKey(m.Certificate); err != nil {
		return err
	}
	if m.Pin != Pin(m.Certificate) {
		return errors.New("vault marker pin does not match its certificate")
	}
	return nil
}

func certificateKey(der []byte) (ed25519.PublicKey, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errors.New("invalid vault certificate")
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("vault certificate must use Ed25519")
	}
	return pub, nil
}

// equal compares markers, ignoring the frontend-only fields.
func (m Marker) equal(o Marker) bool {
	m.Source, o.Source = "", ""
	m.Pending, o.Pending = false, false
	a, errA := json.Marshal(m)
	b, errB := json.Marshal(o)
	return errA == nil && errB == nil && bytes.Equal(a, b)
}

func (r Receipt) signed() []byte {
	r.Signature = nil
	b, err := json.Marshal(r)
	if err != nil {
		return nil
	}
	return append([]byte(receiptContext), b...)
}

// Verify checks the receipt against a trusted marker.
func (r Receipt) Verify(m Marker) error {
	if r.Version != receiptVersion || r.Pin != m.Pin || r.Frontend.UID != m.Frontend.UID {
		return errors.New("vault receipt is for a different vault or account")
	}
	pub, err := certificateKey(m.Certificate)
	if err != nil {
		return err
	}
	msg := r.signed()
	if msg == nil || !ed25519.Verify(pub, msg, r.Signature) {
		return errors.New("vault receipt signature is invalid")
	}
	return nil
}

// validItemPath accepts only the fixed frontend credential locations.
func validItemPath(kind, path string) (scope string, ok bool) {
	parts := strings.Split(path, "/")
	switch kind {
	case ItemAccount:
		return "", path == "admin.json"
	case ItemUpdate:
		return "", path == "update-admin.json"
	case ItemOwner:
		return scopeOf(parts, "clients", "identity.key")
	case ItemTunnelService:
		return scopeOf(parts, "tunnels", "service.json")
	case ItemTunnels:
		return scopeOf(parts, "tunnels", "tunnels.json")
	case ItemTunnelEndpoint:
		return scopeOf(parts, "tunnels", "endpoint.json")
	}
	return "", false
}

func scopeOf(parts []string, dir, file string) (string, bool) {
	if len(parts) != 3 || parts[0] != dir || parts[2] != file || !validScope(parts[1]) {
		return "", false
	}
	return parts[1], true
}

func validScope(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

// adminFile is the frontend admin.json/update-admin.json format.
type adminFile struct {
	Gateway string `json:"gateway"`
	Key     string `json:"key"`
}
