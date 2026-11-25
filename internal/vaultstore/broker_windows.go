//go:build windows

package vaultstore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
)

// errWindowsMigration explains why plaintext never crosses accounts on
// Windows. There is no Windows counterpart of `sudo -u` that connects a pipe
// to a process of the broker account without the everyday account (or
// software running as it) holding the broker's password, and that password
// would void the boundary.
var errWindowsMigration = fmt.Errorf("%w: moving existing plaintext credentials between accounts is not supported on Windows; revoke them at the gateway, stop any retained-tunnel service, delete them from the everyday profile, and provision the vault with fresh credentials", ErrUnsupported)

// DefaultLayout returns the fixed vault location, <system drive>\ProgramData\
// Control Vault. It never consults the environment.
func DefaultLayout() (Layout, error) { return hooks.defaultLayout() }

// MarkerPath is the public marker published by the broker.
func (l Layout) MarkerPath() string { return filepath.Join(l.Root, MarkerName) }

type profile struct {
	Version     int                     `json:"version"`
	State       string                  `json:"state"`
	Marker      Marker                  `json:"marker"`
	Account     client.StandaloneConfig `json:"account"`
	Update      *client.Admin           `json:"update,omitempty"`
	ApprovalKey []byte                  `json:"approvalKey"`
	Exposed     []string                `json:"exposed,omitempty"`
}

type broker struct {
	sid  string
	root *dir
	priv *dir
	lock io.Closer
}

func (b *broker) Close() error {
	var errs []error
	if b.lock != nil {
		errs = append(errs, b.lock.Close())
	}
	errs = append(errs, b.priv.Close(), b.root.Close())
	return errors.Join(errs...)
}

// openRoot verifies the running process, its binary and the fixed root
// without creating or changing anything. The root's single non-administrator
// writer must be this process's account.
func openRoot(layout Layout) (*dir, string, error) {
	canonical, err := requireCanonical(layout.Root)
	if err != nil {
		return nil, "", err
	}
	if err = hooks.checkProcess(); err != nil {
		return nil, "", err
	}
	sid, err := hooks.sid()
	if err != nil {
		return nil, "", err
	}
	if !brokerSID(sid) {
		return nil, "", fmt.Errorf("vault broker must run as a dedicated local or domain user account, not %s", sid)
	}
	if err = hooks.checkExecutable(sid); err != nil {
		return nil, "", err
	}
	root, writer, err := openRootDir(canonical.Root, sid, dirWrite)
	if err != nil {
		return nil, "", err
	}
	if writer != sid {
		_ = root.Close()
		return nil, "", fmt.Errorf("vault root %s is assigned to %s, not to this account (%s)", canonical.Root, writer, sid)
	}
	return root, sid, nil
}

// frontendFor resolves the named everyday account and requires it to be a
// distinct, ordinary user account.
func frontendFor(name, broker string) (Principal, error) {
	frontend, err := hooks.lookup(name)
	if err != nil {
		return Principal{}, err
	}
	if validatePrincipal(frontend) != nil {
		return Principal{}, fmt.Errorf("account %q is not an ordinary user account", name)
	}
	if frontend.UID == broker {
		return Principal{}, errors.New("the everyday account must differ from the vault account running this command")
	}
	return frontend, nil
}

// CheckProvision is a read-only preflight for Provision. Call it before
// soliciting any credential. It verifies the running process token, its
// binary and the fixed root as Provision would, resolves the named everyday
// account, and reports ErrExists for a provisioned profile or ErrBusy for a
// running vault server. It creates no directory, lock or file. Provision
// repeats every check under its lock, so a passing preflight grants nothing.
func CheckProvision(layout Layout, frontendName string) error {
	root, sid, err := openRoot(layout)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err = frontendFor(frontendName, sid); err != nil {
		return err
	}
	if _, err = hooks.lookupSID(sid); err != nil {
		return err
	}
	priv, err := root.sub("private", sid, false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = priv.Close() }()
	if err = requireProtected(priv); err != nil {
		return err
	}
	if err = priv.probeLock("serve.lock"); err != nil {
		return err
	}
	if _, err = priv.readFile("profile.json", sid, true); err == nil {
		return fmt.Errorf("%w; roll back and purge it before provisioning again", ErrExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// openBroker verifies the running process and the vault directories, then
// takes the profile lock so only one broker operation runs at a time.
func openBroker(layout Layout, create bool) (*broker, error) {
	root, sid, err := openRoot(layout)
	if err != nil {
		return nil, err
	}
	priv, err := root.sub("private", sid, create)
	if err == nil {
		err = requireProtected(priv)
	}
	if err != nil {
		_ = priv.Close()
		_ = root.Close()
		return nil, err
	}
	b := &broker{sid: sid, root: root, priv: priv}
	if b.lock, err = priv.lock("serve.lock", sid); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

func (b *broker) readProfile() (*profile, error) {
	data, err := b.priv.readFile("profile.json", b.sid, true)
	if err != nil {
		return nil, err
	}
	var p profile
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&p); err != nil {
		return nil, errors.New("invalid vault profile")
	}
	if p.Version != profileVersion {
		return nil, errors.New("unsupported vault profile version; upgrade this Control binary")
	}
	if err = p.Marker.Validate(); err != nil {
		return nil, err
	}
	if p.Marker.Broker.UID != b.sid {
		return nil, errors.New("vault profile belongs to another account")
	}
	if len(p.ApprovalKey) != ed25519.PublicKeySize || p.Account.Gateway.Key == "" || p.Account.Gateway.URL != p.Marker.Gateway {
		return nil, errors.New("invalid vault profile")
	}
	return &p, nil
}

func (b *broker) writePrivate(d *dir, name string, data []byte) error {
	sd, err := privateSD(b.sid, false)
	if err != nil {
		return err
	}
	return d.writeFile(name, data, sd, false)
}

func (b *broker) writeProfile(p *profile) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return b.writePrivate(b.priv, "profile.json", data)
}

func (b *broker) publishMarker(m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	sd, err := publicSD(b.sid)
	if err != nil {
		return err
	}
	return b.root.writeFile(MarkerName, append(data, '\n'), sd, false)
}

// Provision creates a clean protected profile. It must run as the broker
// account against the fixed root, which an administrator created, owns and
// assigned to that account. It never replaces an existing profile and needs
// no running server.
func Provision(layout Layout, req ProvisionRequest) (Marker, error) {
	if err := validateAddress(req.Address); err != nil {
		return Marker{}, err
	}
	if err := validateSalt(req.Salt); err != nil {
		return Marker{}, err
	}
	if len(req.ApprovalKey) != ed25519.PublicKeySize {
		return Marker{}, errors.New("approval public key must be an Ed25519 public key")
	}
	if req.Account.StateDir != "" {
		return Marker{}, errors.New("vault account state directory is fixed by the vault; leave StateDir empty")
	}
	if err := validateGateway(req.Account.Gateway.URL); err != nil {
		return Marker{}, err
	}
	if strings.TrimSpace(req.Account.Gateway.Key) == "" {
		return Marker{}, errors.New("vault account needs a fresh API key")
	}
	if req.Update != nil && (req.Update.Key == "" || validateGateway(req.Update.URL) != nil) {
		return Marker{}, errors.New("vault update login needs a gateway URL and key")
	}
	b, err := openBroker(layout, true)
	if err != nil {
		return Marker{}, err
	}
	defer func() { _ = b.Close() }()
	brokerPrincipal, err := hooks.lookupSID(b.sid)
	if err != nil {
		return Marker{}, err
	}
	frontend, err := frontendFor(req.Frontend, b.sid)
	if err != nil {
		return Marker{}, err
	}
	if _, err = b.priv.readFile("profile.json", b.sid, true); err == nil {
		return Marker{}, fmt.Errorf("%w; roll back and purge it before provisioning again", ErrExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return Marker{}, err
	}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Marker{}, err
	}
	der, err := certificate(req.Address, pub, key)
	if err != nil {
		return Marker{}, err
	}
	m := Marker{Version: markerVersion, Address: req.Address, Pin: Pin(der), Certificate: der, Salt: slices.Clone(req.Salt),
		Broker: brokerPrincipal, Frontend: frontend, Gateway: req.Account.Gateway.URL, Created: time.Now().UTC().Truncate(time.Second)}
	if err = m.Validate(); err != nil {
		return Marker{}, err
	}
	account := req.Account
	account.AccountRouting = true
	account.PeerOptions = nil
	p := &profile{Version: profileVersion, State: stateActive, Marker: m, Account: account, Update: req.Update, ApprovalKey: slices.Clone(req.ApprovalKey)}
	for _, name := range []string{"clients", "tunnels"} {
		d, err := b.priv.sub(name, b.sid, true)
		if err != nil {
			return Marker{}, err
		}
		_ = d.Close()
	}
	if err = b.writePrivate(b.priv, "tls.key", key); err != nil {
		return Marker{}, err
	}
	// The profile is the commit point; the marker is republished by
	// OpenServer if a crash leaves it missing.
	if err = b.writeProfile(p); err != nil {
		return Marker{}, err
	}
	return m, b.publishMarker(m)
}

func certificate(address string, pub ed25519.PublicKey, key ed25519.PrivateKey) ([]byte, error) {
	host, _, _ := net.SplitHostPort(address)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	// Trust comes from the pin, not validity dates.
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "control-vault"},
		NotBefore: time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC), NotAfter: time.Date(9999, time.December, 31, 23, 59, 59, 0, time.UTC),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP(host)},
	}
	return x509.CreateCertificate(rand.Reader, template, template, pub, key)
}

// Server is an opened protected profile. It holds the profile lock until
// Close, so Provision, Rollback and Purge cannot run concurrently.
type Server struct {
	b       *broker
	p       *profile
	key     ed25519.PrivateKey
	cert    tls.Certificate
	clients string
	tunnels string
}

// OpenServer verifies the broker process, its binary, and every protected
// path, then loads the profile. It runs only as the broker account.
func OpenServer(layout Layout) (*Server, error) {
	b, err := openBroker(layout, false)
	if err != nil {
		return nil, err
	}
	s, err := openServer(b)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	return s, nil
}

func openServer(b *broker) (*Server, error) {
	p, err := b.readProfile()
	if err != nil {
		return nil, err
	}
	if p.State == stateRolledBack {
		return nil, ErrRolledBack
	}
	if p.State != stateActive {
		return nil, errors.New("invalid vault profile state")
	}
	key, err := b.priv.readFile("tls.key", b.sid, true)
	if err != nil {
		return nil, err
	}
	if len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid vault TLS key")
	}
	priv := ed25519.PrivateKey(key)
	pub, err := certificateKey(p.Marker.Certificate)
	if err != nil {
		return nil, err
	}
	if !pub.Equal(priv.Public()) {
		return nil, errors.New("vault TLS key does not match its certificate")
	}
	// Every protected entry the runtime may later open by path must be a
	// private directory or file of this account, with no reparse points.
	if err = checkTree(b.priv, b.sid, 3); err != nil {
		return nil, err
	}
	published, err := b.root.readFile(MarkerName, b.sid, false)
	var current Marker
	if err != nil || json.Unmarshal(published, &current) != nil || !current.equal(p.Marker) {
		if err = b.publishMarker(p.Marker); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"clients", "tunnels"} {
		d, err := b.priv.sub(name, b.sid, true)
		if err != nil {
			return nil, err
		}
		_ = d.Close()
	}
	return &Server{b: b, p: p, key: priv,
		cert:    tls.Certificate{Certificate: [][]byte{p.Marker.Certificate}, PrivateKey: priv},
		clients: filepath.Join(b.priv.path, "clients"), tunnels: filepath.Join(b.priv.path, "tunnels")}, nil
}

func checkTree(d *dir, account string, depth int) error {
	entries, err := d.list()
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(d.path, e.Name())
		switch {
		case e.IsDir():
			if depth <= 1 {
				return fmt.Errorf("unexpected nested directory %s", path)
			}
			child, err := d.sub(e.Name(), account, false)
			if err != nil {
				return err
			}
			err = checkTree(child, account, depth-1)
			_ = child.Close()
			if err != nil {
				return err
			}
		case e.regular():
			h, err := d.openEntry(e.Name(), fileRead)
			if err != nil {
				return err
			}
			err = checkPrivate(h, path, account, false)
			_ = closeHandle(h)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a regular file or directory", path)
		}
	}
	return nil
}

// Close releases the profile lock.
func (s *Server) Close() error { return s.b.Close() }

// Marker returns the published nonsecret marker.
func (s *Server) Marker() Marker { return s.p.Marker }

// Address is the loopback HOST:PORT to listen on.
func (s *Server) Address() string { return s.p.Marker.Address }

// Frontend is the only account the vault serves.
func (s *Server) Frontend() Principal { return s.p.Marker.Frontend }

// TLSConfig serves the pinned certificate over TLS 1.3.
func (s *Server) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{s.cert}}
}

// ApprovalKey verifies frontend approvals. Salt is the matching public salt.
func (s *Server) ApprovalKey() ed25519.PublicKey { return slices.Clone(s.p.ApprovalKey) }

// Salt returns the approval salt published in the marker.
func (s *Server) Salt() []byte { return slices.Clone(s.p.Marker.Salt) }

// Account returns the protected account login. StateDir is the broker's
// private owner directory, so the persisted task owner never leaves it.
func (s *Server) Account() client.StandaloneConfig {
	cfg := s.p.Account
	cfg.StateDir = s.clients
	cfg.AccountRouting = true
	cfg.ICEServers = slices.Clone(cfg.ICEServers)
	return cfg
}

// UpdateAdmin returns the protected superuser login for managed updates.
func (s *Server) UpdateAdmin() (client.Admin, bool) {
	if s.p.Update == nil {
		return client.Admin{}, false
	}
	return *s.p.Update, true
}

// TunnelRoot is the private root for client.WithPersistentTunnels. The
// retained-tunnel service must run as the broker against this root.
func (s *Server) TunnelRoot() string { return s.tunnels }

// Exposures lists protected credentials that were once readable by the
// frontend. Windows never imports them, so the list stays empty.
func (s *Server) Exposures() []string { return slices.Clone(s.p.Exposed) }

// Import is refused on Windows before anything is read or opened; see
// errWindowsMigration. Provision the vault with fresh credentials instead.
func Import(Layout, io.Reader) (Receipt, error) { return Receipt{}, errWindowsMigration }

// Rollback disables the vault. On Windows it never releases protected
// credentials: the returned bundle has no items, and RestoreLegacy only
// clears the frontend activation. Log in again in the everyday account with a
// fresh key, revoke the vault's keys at the gateway, and Purge the profile.
func Rollback(layout Layout) (Bundle, error) {
	b, err := openBroker(layout, false)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = b.Close() }()
	p, err := b.readProfile()
	if err != nil {
		return Bundle{}, err
	}
	if p.State != stateRolledBack {
		p.State = stateRolledBack
		if err = b.writeProfile(p); err != nil {
			return Bundle{}, err
		}
	}
	if err = b.root.rename(MarkerName, MarkerName+".rolled-back"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return Bundle{}, err
	}
	return Bundle{Version: bundleVersion, Kind: BundleRollback, Frontend: p.Marker.Frontend, Items: []BundleItem{}}, nil
}

// Purge deletes a rolled-back profile, including its private keys. It
// refuses an active profile.
func Purge(layout Layout) error {
	b, err := openBroker(layout, false)
	if err != nil {
		return err
	}
	defer func() { _ = b.Close() }()
	p, err := b.readProfile()
	if err != nil {
		return err
	}
	if p.State != stateRolledBack {
		return errors.New("vault profile is active; roll it back first")
	}
	entries, err := b.priv.list()
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.Name() == "serve.lock" || e.Name() == "profile.json":
		case e.IsDir():
			err = b.priv.removeAll(e.Name(), b.sid)
		default:
			err = b.priv.remove(e.Name())
		}
		if err != nil {
			return err
		}
	}
	// The profile goes last so an interrupted purge can be repeated.
	if err = b.priv.remove("profile.json"); err != nil {
		return err
	}
	if err = b.root.remove(MarkerName + ".rolled-back"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// decodeRollback parses a rollback bundle. Windows bundles carry no items.
func decodeRollback(r io.Reader) (*Bundle, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBundle+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxBundle {
		return nil, errors.New("vault bundle is too large")
	}
	var bundle Bundle
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&bundle); err != nil || dec.More() {
		return nil, errors.New("invalid vault bundle")
	}
	if bundle.Version != bundleVersion || bundle.Kind != BundleRollback || validatePrincipal(bundle.Frontend) != nil {
		return nil, fmt.Errorf("expected a version %d %s bundle", bundleVersion, BundleRollback)
	}
	for _, item := range bundle.Items {
		if _, ok := validItemPath(item.Kind, item.Path); !ok {
			return nil, fmt.Errorf("invalid vault bundle entry %q", item.Path)
		}
	}
	if len(bundle.Items) > 0 {
		return nil, fmt.Errorf("%w: restoring protected credentials into the everyday account is not supported on Windows", ErrUnsupported)
	}
	return &bundle, nil
}
