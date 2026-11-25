//go:build darwin || linux

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
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/sys/unix"
)

// DefaultLayout returns the fixed per-platform vault location. It never
// consults the environment.
func DefaultLayout() (Layout, error) { return Layout{Root: defaultRoot}, nil }

// Unix migration must preserve the same login scope as the original tunnel service.
func tunnelScope(cfg client.StandaloneConfig) string {
	return identity.ID([]byte(strings.TrimRight(cfg.Gateway.URL, "/") + "\x00" + cfg.Gateway.Key))
}

// MarkerPath is the public marker published by the broker.
func (l Layout) MarkerPath() string { return filepath.Join(l.Root, MarkerName) }

type profile struct {
	Version     int                     `json:"version"`
	State       string                  `json:"state"`
	Marker      Marker                  `json:"marker"`
	Account     client.StandaloneConfig `json:"account"`
	Update      *client.Admin           `json:"update,omitempty"`
	ApprovalKey []byte                  `json:"approvalKey"`
	// Exposed lists protected credentials that were previously stored in
	// the everyday profile. They cannot be made secret again by moving them.
	Exposed []string `json:"exposed,omitempty"`
}

type broker struct {
	uid  int
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

// openRoot verifies the running process, its binary and the layout root
// without creating or changing anything.
func openRoot(layout Layout) (*dir, int, error) {
	if err := hooks.checkProcess(); err != nil {
		return nil, 0, err
	}
	uid := hooks.uid()
	if err := hooks.checkExecutable(uid); err != nil {
		return nil, 0, err
	}
	root, err := openTrusted(layout.Root, uid)
	if err != nil {
		return nil, 0, err
	}
	var st unix.Stat_t
	if err = unix.Fstat(root.fd, &st); err == nil && int(st.Uid) != uid {
		err = fmt.Errorf("vault root %s must be owned by the vault account (uid %d)", layout.Root, uid)
	}
	if err != nil {
		_ = root.Close()
		return nil, 0, err
	}
	return root, uid, nil
}

// frontendFor resolves the named everyday account and requires it to be a
// distinct, non-root account.
func frontendFor(name string, broker int) (Principal, error) {
	frontend, err := hooks.lookup(name)
	if err != nil {
		return Principal{}, err
	}
	if validatePrincipal(frontend) != nil {
		return Principal{}, fmt.Errorf("account %q has no usable uid", name)
	}
	if frontend.UID == "0" {
		return Principal{}, errors.New("the everyday account must not be root")
	}
	if frontend.UID == strconv.Itoa(broker) {
		return Principal{}, errors.New("the everyday account must differ from the vault account running this command")
	}
	return frontend, nil
}

// CheckProvision is a read-only preflight for Provision. Call it before
// soliciting any credential. It verifies the running process, its binary and
// the layout root as Provision would, resolves the named everyday account,
// and reports ErrExists for a provisioned profile or ErrBusy for a running
// vault server. It creates no directory, lock or file. Provision repeats every
// check under its lock, so a passing preflight grants nothing.
func CheckProvision(layout Layout, frontendName string) error {
	root, uid, err := openRoot(layout)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if _, err = frontendFor(frontendName, uid); err != nil {
		return err
	}
	if _, err = hooks.lookupID(uid); err != nil {
		return err
	}
	priv, err := root.sub("private", uid, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = priv.Close() }()
	if err = priv.probeLock("serve.lock"); err != nil {
		return err
	}
	if _, err = priv.readFile("profile.json", uid, true); err == nil {
		return fmt.Errorf("%w; roll back and purge it before provisioning again", ErrExists)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// openBroker verifies the running process and the vault directories, then
// takes the profile lock so only one broker operation runs at a time.
func openBroker(layout Layout, create bool) (*broker, error) {
	root, uid, err := openRoot(layout)
	if err != nil {
		return nil, err
	}
	priv, err := root.sub("private", uid, create)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	b := &broker{uid: uid, root: root, priv: priv}
	if b.lock, err = priv.lock("serve.lock", uid); err != nil {
		_ = b.Close()
		return nil, err
	}
	return b, nil
}

func (b *broker) readProfile() (*profile, error) {
	data, err := b.priv.readFile("profile.json", b.uid, true)
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
	if p.Marker.Broker.UID != strconv.Itoa(b.uid) {
		return nil, errors.New("vault profile belongs to another account")
	}
	if len(p.ApprovalKey) != ed25519.PublicKeySize || p.Account.Gateway.Key == "" || p.Account.Gateway.URL != p.Marker.Gateway {
		return nil, errors.New("invalid vault profile")
	}
	return &p, nil
}

func (b *broker) writeProfile(p *profile) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return b.priv.writeFile("profile.json", data, 0o600, false)
}

func (b *broker) publishMarker(m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return b.root.writeFile(MarkerName, append(data, '\n'), 0o644, false)
}

// Provision creates a clean protected profile. It must run as the broker
// account in the Layout root, which an administrator created owned by that
// account. It never replaces an existing profile and needs no running server.
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
	brokerPrincipal, err := hooks.lookupID(b.uid)
	if err != nil {
		return Marker{}, err
	}
	frontend, err := frontendFor(req.Frontend, b.uid)
	if err != nil {
		return Marker{}, err
	}
	if _, err = b.priv.readFile("profile.json", b.uid, true); err == nil {
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
		d, err := b.priv.sub(name, b.uid, true)
		if err != nil {
			return Marker{}, err
		}
		_ = d.Close()
	}
	if err = b.priv.writeFile("tls.key", key, 0o600, false); err != nil {
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
// Close, so Provision, Import, Rollback and Purge cannot run concurrently.
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
	key, err := b.priv.readFile("tls.key", b.uid, true)
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
	// private directory or file of this account, with no links.
	if err = checkTree(b.priv, b.uid, 3); err != nil {
		return nil, err
	}
	published, err := b.root.readFile(MarkerName, b.uid, false)
	var current Marker
	if err != nil || json.Unmarshal(published, &current) != nil || !current.equal(p.Marker) {
		if err = b.publishMarker(p.Marker); err != nil {
			return nil, err
		}
	}
	for _, name := range []string{"clients", "tunnels"} {
		d, err := b.priv.sub(name, b.uid, true)
		if err != nil {
			return nil, err
		}
		_ = d.Close()
	}
	return &Server{b: b, p: p, key: priv,
		cert:    tls.Certificate{Certificate: [][]byte{p.Marker.Certificate}, PrivateKey: priv},
		clients: filepath.Join(b.priv.path, "clients"), tunnels: filepath.Join(b.priv.path, "tunnels")}, nil
}

func checkTree(d *dir, owner, depth int) error {
	entries, err := d.list()
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch {
		case e.IsDir():
			if depth <= 1 {
				return fmt.Errorf("unexpected nested directory %s", filepath.Join(d.path, e.Name()))
			}
			child, err := d.sub(e.Name(), owner, false)
			if err != nil {
				return err
			}
			err = checkTree(child, owner, depth-1)
			_ = child.Close()
			if err != nil {
				return err
			}
		case e.Type().IsRegular():
			fd, err := unix.Openat(d.fd, e.Name(), unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
			if err != nil {
				return err
			}
			err = checkPrivate(fd, filepath.Join(d.path, e.Name()), owner, false)
			_ = unix.Close(fd)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is not a regular file or directory", filepath.Join(d.path, e.Name()))
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
// frontend. They should be rotated; moving them did not make them secret.
func (s *Server) Exposures() []string { return slices.Clone(s.p.Exposed) }

// Import moves a frontend migration bundle into the protected profile. It
// runs as the broker while the vault server is stopped, and is all or
// nothing: any item that cannot be handled safely rejects the whole bundle
// before anything is written. The account key equal to the provisioned login
// and the superuser key are carried with their exposure recorded; a different
// key for the same gateway is reported for revocation. Task owner keys and
// the account login's retained tunnel definitions are carried.
func Import(layout Layout, r io.Reader) (Receipt, error) {
	b, err := openBroker(layout, false)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = b.Close() }()
	p, err := b.readProfile()
	if err != nil {
		return Receipt{}, err
	}
	if p.State != stateActive {
		return Receipt{}, ErrRolledBack
	}
	bundle, err := decodeBundle(r, BundleMigration)
	if err != nil {
		return Receipt{}, err
	}
	if bundle.Frontend.UID != p.Marker.Frontend.UID {
		return Receipt{}, errors.New("migration bundle is from a different account than this vault serves")
	}
	key, err := b.priv.readFile("tls.key", b.uid, true)
	if err != nil {
		return Receipt{}, err
	}
	plan, err := planImport(b, p, bundle)
	if err != nil {
		return Receipt{}, err
	}
	for _, w := range plan.writes {
		if err = w.apply(b); err != nil {
			return Receipt{}, fmt.Errorf("import partially written; the frontend copy is unchanged, rerun import: %w", err)
		}
	}
	for _, w := range plan.writes {
		if err = w.verify(b); err != nil {
			return Receipt{}, err
		}
	}
	for _, e := range plan.exposed {
		if !slices.Contains(p.Exposed, e) {
			p.Exposed = append(p.Exposed, e)
		}
	}
	if plan.update != nil {
		p.Update = plan.update
	}
	if err = b.writeProfile(p); err != nil {
		return Receipt{}, err
	}
	receipt := Receipt{Version: receiptVersion, Pin: p.Marker.Pin, Frontend: p.Marker.Frontend, Items: plan.items,
		RevokeRequired: plan.revoke, Issued: time.Now().UTC().Truncate(time.Second)}
	receipt.Signature = ed25519.Sign(ed25519.PrivateKey(key), receipt.signed())
	return receipt, nil
}

func decodeBundle(r io.Reader, kind string) (*Bundle, error) {
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
	if bundle.Version != bundleVersion || bundle.Kind != kind {
		return nil, fmt.Errorf("expected a version %d %s bundle", bundleVersion, kind)
	}
	if validatePrincipal(bundle.Frontend) != nil || len(bundle.Items) > 1024 {
		return nil, errors.New("invalid vault bundle")
	}
	seen := map[string]bool{}
	for _, item := range bundle.Items {
		if _, ok := validItemPath(item.Kind, item.Path); !ok || seen[item.Path] {
			return nil, fmt.Errorf("invalid vault bundle entry %q", item.Path)
		}
		seen[item.Path] = true
		if item.Digest != digest(item.Data) || len(item.Data) > maxFileSize {
			return nil, fmt.Errorf("vault bundle entry %s does not match its digest", item.Path)
		}
	}
	return &bundle, nil
}

type protectedWrite struct {
	dirs []string
	name string
	data []byte
}

func (w protectedWrite) open(b *broker, create bool) (*dir, error) {
	if len(w.dirs) == 0 {
		return nil, errors.New("protected files live in a profile subdirectory")
	}
	d := b.priv
	var opened []*dir
	for _, name := range w.dirs {
		next, err := d.sub(name, b.uid, create)
		if err != nil {
			for _, o := range opened {
				_ = o.Close()
			}
			return nil, err
		}
		opened = append(opened, next)
		d = next
	}
	for _, o := range opened[:len(opened)-1] {
		_ = o.Close()
	}
	return d, nil
}

func (w protectedWrite) read(b *broker) ([]byte, error) {
	d, err := w.open(b, false)
	if errors.Is(err, unix.ENOENT) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.readFile(w.name, b.uid, true)
}

func (w protectedWrite) apply(b *broker) error {
	d, err := w.open(b, true)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.writeFile(w.name, w.data, 0o600, false)
}

func (w protectedWrite) verify(b *broker) error {
	data, err := w.read(b)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, w.data) {
		return fmt.Errorf("protected copy %s/%s did not verify", strings.Join(w.dirs, "/"), w.name)
	}
	return nil
}

type importPlan struct {
	writes  []protectedWrite
	items   []ReceiptItem
	revoke  []string
	exposed []string
	// update is a superuser login carried into a profile that had none.
	update *client.Admin
}

// planImport decides every item before anything is written. Credentials
// equal to the protected login are carried and recorded as exposed: moving a
// key does not undo an earlier compromise, so rotation is recommended. A
// different key for the same gateway is superseded and must be revoked.
// Retained tunnels are carried only from the exact login scope of the
// protected account, so definitions are never rebound to another login on a
// gateway-URL match or a claim inside the bundle. Anything else aborts.
func planImport(b *broker, p *profile, bundle *Bundle) (*importPlan, error) {
	plan := &importPlan{}
	var rejected []string
	reject := func(path, why string) { rejected = append(rejected, path+": "+why) }
	accountURL := strings.TrimRight(p.Account.Gateway.URL, "/")
	accountScope := tunnelScope(p.Account)
	revoked := map[string]bool{}
	expose := func(what, gateway string) {
		note := what + " for " + gateway + " was stored in the everyday profile; rotate it when possible"
		if !slices.Contains(plan.exposed, note) {
			plan.exposed = append(plan.exposed, note)
		}
	}
	supersede := func(path, gateway, key, what string) {
		if !revoked[key] {
			revoked[key] = true
			plan.revoke = append(plan.revoke, fmt.Sprintf("%s for %s (from %s)", what, gateway, path))
		}
	}
	byKind := func(kind string) []BundleItem {
		var out []BundleItem
		for _, item := range bundle.Items {
			if item.Kind == kind {
				out = append(out, item)
			}
		}
		return out
	}
	dispositions := map[string]string{}

	// The superuser login first: login saves it in admin.json as well.
	update := p.Update
	for _, item := range byKind(ItemUpdate) {
		var a adminFile
		if json.Unmarshal(item.Data, &a) != nil || a.Key == "" || validateGateway(a.Gateway) != nil {
			reject(item.Path, "invalid login file")
			continue
		}
		switch {
		case update == nil:
			update = &client.Admin{URL: a.Gateway, Key: a.Key}
			plan.update = update
			expose("superuser API key", a.Gateway)
			dispositions[item.Path] = Imported
		case update.Key == a.Key:
			expose("superuser API key", a.Gateway)
			dispositions[item.Path] = Imported
		default:
			supersede(item.Path, a.Gateway, a.Key, "superuser API key")
			dispositions[item.Path] = Superseded
		}
	}
	for _, item := range byKind(ItemAccount) {
		var a adminFile
		if json.Unmarshal(item.Data, &a) != nil || a.Key == "" {
			reject(item.Path, "invalid login file")
			continue
		}
		switch {
		case a.Key == p.Account.Gateway.Key:
			expose("account API key", a.Gateway)
			dispositions[item.Path] = Imported
		case update != nil && a.Key == update.Key:
			dispositions[item.Path] = Imported
		case strings.TrimRight(a.Gateway, "/") == accountURL:
			supersede(item.Path, a.Gateway, a.Key, "account API key")
			dispositions[item.Path] = Superseded
		default:
			reject(item.Path, "login for another gateway; it is not served by this vault")
		}
	}
	for _, item := range byKind(ItemOwner) {
		scope, _ := validItemPath(item.Kind, item.Path)
		if len(item.Data) != ed25519.PrivateKeySize || !bytes.Equal(ed25519.NewKeyFromSeed(item.Data[:ed25519.SeedSize]), item.Data) {
			reject(item.Path, "invalid task owner key")
			continue
		}
		w := protectedWrite{dirs: []string{"clients", scope}, name: "identity.key", data: item.Data}
		if existing, err := w.read(b); err == nil && !bytes.Equal(existing, item.Data) {
			reject(item.Path, "a different task owner is already stored for this gateway account")
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		plan.writes = append(plan.writes, w)
		owner := ed25519.PrivateKey(item.Data).Public().(ed25519.PublicKey)
		expose("task owner "+Pin(owner)[:16], "its account")
		dispositions[item.Path] = Imported
	}
	for _, item := range byKind(ItemTunnelService) {
		scope, _ := validItemPath(item.Kind, item.Path)
		var cfg client.StandaloneConfig
		if json.Unmarshal(item.Data, &cfg) != nil || cfg.Gateway.Key == "" || tunnelScope(cfg) != scope {
			reject(item.Path, "invalid retained-tunnel login")
			continue
		}
		if scope != accountScope {
			reject(item.Path, "retained-tunnel login differs from the vault account login; dispose those tunnels with the legacy CLI first")
			continue
		}
		// The same key as the protected account: nothing else to keep.
		dispositions[item.Path] = Discarded
	}
	for _, item := range byKind(ItemTunnels) {
		scope, _ := validItemPath(item.Kind, item.Path)
		var infos []client.PersistentTunnelInfo
		if json.Unmarshal(item.Data, &infos) != nil || client.ValidateRetainedTunnelState(infos) != nil {
			reject(item.Path, "invalid retained tunnel definitions")
			continue
		}
		if len(infos) == 0 {
			dispositions[item.Path] = Discarded
			continue
		}
		if scope != accountScope {
			reject(item.Path, "retained tunnels belong to a different login than the vault account; dispose them with the legacy CLI first")
			continue
		}
		// Definitions are kept byte for byte, preserving targets and expiry.
		w := protectedWrite{dirs: []string{"tunnels", scope}, name: "tunnels.json", data: item.Data}
		if existing, err := w.read(b); err == nil && !bytes.Equal(existing, item.Data) {
			reject(item.Path, "the vault already holds different retained tunnels")
			continue
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		plan.writes = append(plan.writes, w)
		dispositions[item.Path] = Imported
	}
	for _, item := range byKind(ItemTunnelEndpoint) {
		// A management token for a stopped user service; export refuses a
		// running one.
		dispositions[item.Path] = Discarded
	}
	if len(rejected) > 0 {
		return nil, fmt.Errorf("migration rejected; nothing was written:\n  %s", strings.Join(rejected, "\n  "))
	}
	for _, item := range bundle.Items {
		plan.items = append(plan.items, ReceiptItem{Kind: item.Kind, Path: item.Path, Digest: item.Digest, Disposition: dispositions[item.Path]})
	}
	return plan, nil
}

// Rollback disables the vault and returns its credentials as a plaintext
// bundle for RestoreLegacy in the frontend account. The protected copies stay
// in place until Purge, so an interrupted rollback can be repeated. The
// credentials are exposed to the frontend again; rotate them afterwards.
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
	bundle := Bundle{Version: bundleVersion, Kind: BundleRollback, Frontend: p.Marker.Frontend}
	add := func(kind, path string, data []byte) {
		bundle.Items = append(bundle.Items, BundleItem{Kind: kind, Path: path, Digest: digest(data), Data: data})
	}
	account, err := json.Marshal(adminFile{Gateway: p.Account.Gateway.URL, Key: p.Account.Gateway.Key})
	if err != nil {
		return Bundle{}, err
	}
	add(ItemAccount, "admin.json", account)
	if p.Update != nil {
		update, err := json.Marshal(adminFile{Gateway: p.Update.URL, Key: p.Update.Key})
		if err != nil {
			return Bundle{}, err
		}
		add(ItemUpdate, "update-admin.json", update)
	}
	accountScope := tunnelScope(p.Account)
	for _, area := range []struct{ dir, file, kind string }{{"clients", "identity.key", ItemOwner}, {"tunnels", "tunnels.json", ItemTunnels}} {
		d, err := b.priv.sub(area.dir, b.uid, false)
		if err != nil {
			return Bundle{}, err
		}
		entries, err := d.list()
		_ = d.Close()
		if err != nil {
			return Bundle{}, err
		}
		for _, e := range entries {
			if !e.IsDir() || !validScope(e.Name()) {
				continue
			}
			data, err := protectedWrite{dirs: []string{area.dir, e.Name()}, name: area.file}.read(b)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return Bundle{}, err
			}
			if area.kind == ItemTunnels {
				if e.Name() != accountScope {
					return Bundle{}, fmt.Errorf("protected tunnels %s do not belong to the vault account", e.Name())
				}
				// A legacy user tunnel service needs its saved login; the
				// frontend sets the state directory on restore.
				service := p.Account
				service.StateDir = ""
				service.AccountRouting = true
				cfg, err := json.Marshal(service)
				if err != nil {
					return Bundle{}, err
				}
				add(ItemTunnelService, "tunnels/"+e.Name()+"/service.json", cfg)
			}
			add(area.kind, area.dir+"/"+e.Name()+"/"+area.file, data)
		}
	}
	if p.State != stateRolledBack {
		p.State = stateRolledBack
		if err = b.writeProfile(p); err != nil {
			return Bundle{}, err
		}
	}
	if err = b.root.rename(MarkerName, MarkerName+".rolled-back"); err != nil && !errors.Is(err, unix.ENOENT) {
		return Bundle{}, err
	}
	return bundle, nil
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
			err = b.priv.removeAll(e.Name(), b.uid)
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
	if err = b.root.remove(MarkerName + ".rolled-back"); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
