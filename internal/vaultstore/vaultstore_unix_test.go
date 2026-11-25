//go:build darwin || linux

package vaultstore

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/sys/unix"
)

// The test process plays both accounts: kernel ownership checks see its real
// uid as the broker, and hooks present a second, fictitious frontend uid.
type fixture struct {
	t        *testing.T
	base     string
	layout   Layout
	home     string
	broker   int
	frontend int
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	if unix.Geteuid() == 0 {
		t.Skip("ownership tests need a non-root test account")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, base: base, layout: Layout{Root: filepath.Join(base, "vault")}, home: filepath.Join(base, "home"), broker: unix.Geteuid(), frontend: unix.Geteuid() + 4242}
	for _, d := range []string{f.layout.Root, f.home} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	saved := hooks
	t.Cleanup(func() { hooks = saved })
	real := saved.lookupID
	hooks.checkProcess = func() error { return nil }
	hooks.checkExecutable = func(int) error { return nil }
	hooks.defaultLayout = func() Layout { return Layout{Root: filepath.Join(base, "no-default")} }
	hooks.lookup = func(name string) (Principal, error) {
		return Principal{UID: strconv.Itoa(f.frontend), Name: name}, nil
	}
	hooks.lookupID = func(uid int) (Principal, error) {
		if uid == f.frontend {
			return Principal{UID: strconv.Itoa(uid), Name: "everyday"}, nil
		}
		return real(uid)
	}
	f.asBroker()
	return f
}

func (f *fixture) asBroker()   { hooks.uid = func() int { return f.broker } }
func (f *fixture) asFrontend() { hooks.uid = func() int { return f.frontend } }

func (f *fixture) provision(key string) Marker {
	f.t.Helper()
	f.asBroker()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	m, err := Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: key}},
		ApprovalKey: pub, Salt: bytes.Repeat([]byte{7}, 16)})
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) write(rel string, data []byte) {
	f.t.Helper()
	path := filepath.Join(f.home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestProvisionServeAndActivate(t *testing.T) {
	f := newFixture(t)
	m := f.provision("fresh")
	if m.Pin != Pin(m.Certificate) || m.Broker.UID != strconv.Itoa(f.broker) || m.Frontend.UID != strconv.Itoa(f.frontend) {
		t.Fatalf("marker = %+v", m)
	}
	s, err := OpenServer(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := s.Account(); cfg.StateDir != filepath.Join(f.layout.Root, "private", "clients") || !cfg.AccountRouting || cfg.Gateway.Key != "fresh" {
		t.Fatalf("account = %+v", cfg)
	}
	if _, err := OpenServer(f.layout); !errors.Is(err, ErrBusy) {
		t.Fatalf("second server: %v", err)
	}
	if _, err := Import(f.layout, bytes.NewReader(nil)); !errors.Is(err, ErrBusy) {
		t.Fatalf("import while serving: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:1", Frontend: "x", Account: client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}}, ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)}); !errors.Is(err, ErrExists) {
		t.Fatalf("reprovision: %v", err)
	}
	// The private directory is 0700 and the published marker carries no secret.
	info, err := os.Stat(filepath.Join(f.layout.Root, "private"))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("private mode: %v %v", info, err)
	}
	published, _ := os.ReadFile(f.layout.MarkerPath())
	if bytes.Contains(published, []byte("fresh")) {
		t.Fatal("marker contains the account key")
	}

	f.asFrontend()
	if _, err := LoadMarker(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("before activation: %v", err)
	}
	local, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath()})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMarker(f.home)
	if err != nil || loaded.Pin != m.Pin || loaded.Source != local.Source {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	// A lookalike written by the frontend into its own copy is rejected.
	local.Address = "127.0.0.1:9999"
	data, _ := json.Marshal(local)
	f.write(MarkerName, data)
	if _, err := LoadMarker(f.home); err == nil {
		t.Fatal("tampered activation accepted")
	}
}

func TestMarkerOwnedByFrontendIsRejected(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	// The same files, but the caller is the account that owns them: anything
	// the frontend can write must never become a trusted marker.
	hooks.uid = func() int { return f.broker }
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath()}); err == nil {
		t.Fatal("self-owned marker accepted")
	}
	f.asFrontend()
	lookalike := filepath.Join(f.home, "fake.json")
	published, _ := os.ReadFile(f.layout.MarkerPath())
	if err := os.WriteFile(lookalike, published, 0o644); err != nil {
		t.Fatal(err)
	}
	// Owned by the test's real uid, which here stands for a third account
	// with a writable chain: the home directory is writable by its owner only,
	// but the marker must also name that owner as broker and pass the walk.
	if err := os.Chmod(f.home, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(filepath.Join(f.base, "other"), ActivateOptions{Source: lookalike}); err == nil {
		t.Fatal("marker in a writable directory accepted")
	}
}

func TestTrustedWalkRejectsSubstitution(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")

	link := filepath.Join(f.base, "link")
	if err := os.Symlink(f.layout.Root, link); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(Layout{Root: link}); err == nil {
		t.Fatal("symlinked root accepted")
	}
	f.asFrontend()
	if _, err := Activate(f.home, ActivateOptions{Source: filepath.Join(link, MarkerName)}); err == nil {
		t.Fatal("marker through a symlink accepted")
	}
	f.asBroker()

	for _, mode := range []os.FileMode{0o777, 0o775, 0o777 | os.ModeSticky} {
		if err := os.Chmod(f.base, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenServer(f.layout); err == nil {
			t.Fatalf("ancestor mode %v accepted", mode)
		}
	}
	if err := os.Chmod(f.base, 0o755); err != nil {
		t.Fatal(err)
	}
	// Only the broker may own the chain; any other uid is a substitution.
	hooks.uid = func() int { return f.broker + 1 }
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("chain owned by another account accepted")
	}
	if err := checkExecutable(f.broker + 1); err == nil {
		t.Fatal("binary owned by another account accepted")
	}
	f.asBroker()

	private := filepath.Join(f.layout.Root, "private")
	if err := os.Chmod(private, 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("group-readable private directory accepted")
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(private, "clients", "planted")
	if err := os.Symlink(f.home, planted); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("symlink inside the private profile accepted")
	}
	if err := os.Remove(planted); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(private, "tls.key"), filepath.Join(f.base, "tls.copy")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("hard-linked private key accepted")
	}
	if err := os.Remove(filepath.Join(f.base, "tls.copy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(private, "profile.json"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("group-readable profile accepted")
	}
}

type legacy struct {
	ownerScope, tunnelScope string
	owner                   ed25519.PrivateKey
	tunnels                 []byte
}

func (f *fixture) legacyState(key string) legacy {
	f.t.Helper()
	_, owner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	l := legacy{ownerScope: identity.ID([]byte("https://gw.example\x00user")), owner: owner}
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: key}, StateDir: filepath.Join(f.home, "clients"), AccountRouting: true}
	l.tunnelScope = tunnelScope(cfg)
	service, _ := json.Marshal(cfg)
	tunnels, _ := json.Marshal([]client.PersistentTunnelInfo{{PersistentTunnelSpec: client.PersistentTunnelSpec{ID: "db", ForwardSpec: client.ForwardSpec{Node: "n1", Listen: "127.0.0.1:5432", Address: "127.0.0.1:5432"}}, NodeID: string(bytes.Repeat([]byte("a"), 64)), Created: time.Now().UTC()}})
	admin, _ := json.Marshal(adminFile{Gateway: "https://gw.example", Key: key})
	f.write("admin.json", admin)
	f.write("clients/"+l.ownerScope+"/identity.key", owner)
	f.write("tunnels/"+l.tunnelScope+"/service.json", service)
	f.write("tunnels/"+l.tunnelScope+"/tunnels.json", tunnels)
	l.tunnels = tunnels
	f.write("tunnels/"+l.tunnelScope+"/endpoint.json", []byte(`{"address":"127.0.0.1:1","token":"t"}`))
	return l
}

func (f *fixture) migrate() Receipt {
	f.t.Helper()
	f.asFrontend()
	bundle, err := ExportMigration(f.home)
	if err != nil {
		f.t.Fatal(err)
	}
	data, _ := json.Marshal(bundle)
	f.asBroker()
	receipt, err := Import(f.layout, bytes.NewReader(data))
	if err != nil {
		f.t.Fatal(err)
	}
	f.asFrontend()
	return receipt
}

func TestMigrationDeletesPlaintextOnlyAfterVerifiedReceipt(t *testing.T) {
	f := newFixture(t)
	l := f.legacyState("current")
	f.write("update-admin.json", []byte(`{"gateway":"https://gw.example","key":"super"}`))
	m := f.provision("current")
	receipt := f.migrate()
	if len(receipt.RevokeRequired) != 0 {
		t.Fatalf("revocations = %v", receipt.RevokeRequired)
	}
	admin := filepath.Join(f.home, "admin.json")
	activate := func(r *Receipt) error {
		_, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath(), Receipt: r})
		return err
	}

	if err := activate(nil); !errors.Is(err, ErrAmbientCredentials) || !exists(admin) {
		t.Fatalf("activation without receipt: %v", err)
	}
	forged := receipt
	forged.Items = append([]ReceiptItem(nil), receipt.Items...)
	forged.Items[0].Digest = digest([]byte("other"))
	if err := activate(&forged); err == nil || !exists(admin) {
		t.Fatalf("forged receipt: %v", err)
	}
	// Plaintext created after the export is not covered.
	later := identity.ID([]byte("later"))
	f.write("clients/"+later+"/identity.key", l.owner)
	if err := activate(&receipt); !errors.Is(err, ErrIncomplete) || !exists(admin) {
		t.Fatalf("uncovered file: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(f.home, "clients", later)); err != nil {
		t.Fatal(err)
	}
	// Node tokens and credential environment block before any removal.
	f.write("node.json", []byte(`{"token":"node"}`))
	if err := activate(&receipt); !errors.Is(err, ErrAmbientCredentials) || !exists(admin) {
		t.Fatalf("node token: %v", err)
	}
	if err := os.Chmod(filepath.Join(f.home, "node.json"), 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_TOKEN", "node")
	if err := activate(&receipt); !errors.Is(err, ErrAmbientCredentials) || !exists(admin) {
		t.Fatalf("CONTROL_TOKEN: %v", err)
	}
	t.Setenv("CONTROL_TOKEN", "")
	// A running tunnel service blocks without touching anything.
	lock := flock.New(filepath.Join(f.home, "tunnels", l.tunnelScope, "service.lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatal(err)
	}
	if err := activate(&receipt); !errors.Is(err, ErrBusy) || !exists(admin) {
		t.Fatalf("live tunnel service: %v", err)
	}
	_ = lock.Close()

	// An interrupted activation leaves a pending marker that blocks both
	// plaintext and vault mode until it is resumed with the same receipt.
	pending := m
	pending.Source, pending.Pending = f.layout.MarkerPath(), true
	data, _ := json.Marshal(pending)
	f.write(MarkerName, data)
	if err := os.Remove(admin); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending activation: %v", err)
	}
	if err := activate(&receipt); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"admin.json", "update-admin.json", "clients/" + l.ownerScope + "/identity.key", "tunnels/" + l.tunnelScope + "/service.json", "tunnels/" + l.tunnelScope + "/tunnels.json", "tunnels/" + l.tunnelScope + "/endpoint.json"} {
		if exists(filepath.Join(f.home, rel)) {
			t.Fatalf("%s retained", rel)
		}
	}
	if err := CheckAmbient(f.home); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadMarker(f.home); err != nil || got.Pin != m.Pin || got.Pending {
		t.Fatalf("load: %+v %v", got, err)
	}
	// Credentials reappearing in the everyday profile stop vault mode.
	f.write("admin.json", []byte(`{"gateway":"https://gw.example","key":"again"}`))
	if _, err := LoadMarker(f.home); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("ambient after activation: %v", err)
	}
	if err := os.Remove(admin); err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(f.base, "selected.json")
	if _, err := LoadMarker(f.home, selected); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(selected, []byte(`{"token":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home, selected); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("selected node config: %v", err)
	}

	f.asBroker()
	s, err := OpenServer(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	owner, err := os.ReadFile(filepath.Join(s.Account().StateDir, l.ownerScope, "identity.key"))
	if err != nil || !bytes.Equal(owner, l.owner) {
		t.Fatalf("owner key: %v", err)
	}
	scope := tunnelScope(s.Account())
	if scope != l.tunnelScope {
		t.Fatal("tunnel scope changed")
	}
	if got, err := os.ReadFile(filepath.Join(s.TunnelRoot(), scope, "tunnels.json")); err != nil || !bytes.Equal(got, l.tunnels) {
		t.Fatalf("tunnels not kept byte for byte: %v", err)
	}
	for _, name := range []string{"service.json", "endpoint.json"} {
		if exists(filepath.Join(s.TunnelRoot(), scope, name)) {
			t.Fatalf("protected tunnel scope has %s", name)
		}
	}
	if update, ok := s.UpdateAdmin(); !ok || update.Key != "super" {
		t.Fatalf("update login = %+v %v", update, ok)
	}
	if len(s.Exposures()) != 3 {
		t.Fatalf("exposures = %v", s.Exposures())
	}
}

func TestImportRejectsForeignTunnelsAndSupersedesOtherKeys(t *testing.T) {
	f := newFixture(t)
	l := f.legacyState("old")
	f.provision("fresh")
	f.asFrontend()
	bundle, err := ExportMigration(f.home)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(bundle)
	f.asBroker()
	// Tunnels of another login are never rebound on a gateway URL match.
	if _, err := Import(f.layout, bytes.NewReader(data)); err == nil {
		t.Fatal("tunnels of another login accepted")
	}
	if exists(filepath.Join(f.layout.Root, "private", "clients", l.ownerScope)) {
		t.Fatal("rejected import wrote an owner key")
	}
	// A tampered item fails its digest.
	bundle.Items[0].Data = []byte(`{"gateway":"https://gw.example","key":"other"}`)
	data, _ = json.Marshal(bundle)
	if _, err := Import(f.layout, bytes.NewReader(data)); err == nil {
		t.Fatal("tampered bundle accepted")
	}
	if err := os.RemoveAll(filepath.Join(f.home, "tunnels")); err != nil {
		t.Fatal(err)
	}
	receipt := f.migrate()
	if len(receipt.RevokeRequired) != 1 {
		t.Fatalf("revocations = %v", receipt.RevokeRequired)
	}
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath(), Receipt: &receipt}); err == nil || !exists(filepath.Join(f.home, "admin.json")) {
		t.Fatalf("activation without revocation: %v", err)
	}
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath(), Receipt: &receipt, RevocationConfirmed: true}); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMarkerLegacyHomesNeedNoTrust(t *testing.T) {
	f := newFixture(t)
	// Symlinked, nested, missing and permissive homes without any activation
	// entry stay in legacy mode without strict checks.
	link := filepath.Join(f.base, "link")
	if err := os.Symlink(f.home, link); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(link, "nested", "home")
	if err := os.MkdirAll(nested, 0o777); err != nil {
		t.Fatal(err)
	}
	f.write("admin.json", []byte(`{"gateway":"https://gw.example","key":"k"}`))
	for _, home := range []string{f.home, link, nested, filepath.Join(f.base, "missing")} {
		if _, err := LoadMarker(home); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: %v", home, err)
		}
	}
}

func TestLoadMarkerFallsBackOnlyOnPositiveAbsence(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	f.asFrontend()
	hooks.defaultLayout = func() Layout { return f.layout }
	// A verified default marker for this account without local activation.
	if _, err := LoadMarker(f.home); !errors.Is(err, ErrActivationMissing) {
		t.Fatalf("missing activation: %v", err)
	}
	// A marker serving another account is a positive "not for us".
	other := f.frontend
	f.frontend++
	if _, err := LoadMarker(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("other account: %v", err)
	}
	f.frontend = other
	// A dangling link at the fixed path is not an absence.
	dangling := Layout{Root: filepath.Join(f.base, "dangling")}
	if err := os.Mkdir(dangling.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(f.base, "nowhere"), dangling.MarkerPath()); err != nil {
		t.Fatal(err)
	}
	hooks.defaultLayout = func() Layout { return dangling }
	if _, err := LoadMarker(f.home); err == nil || errors.Is(err, os.ErrNotExist) || !errors.Is(err, ErrUnverified) {
		t.Fatalf("dangling default marker: %v", err)
	}
	// Nor is a dangling local activation, or one whose source vanished.
	if err := os.Symlink(filepath.Join(f.base, "nowhere"), filepath.Join(f.home, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dangling local activation: %v", err)
	}
	if err := os.Remove(filepath.Join(f.home, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath()}); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.layout.MarkerPath(), f.layout.MarkerPath()+".moved"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("vanished source: %v", err)
	}
}

func TestExportRefusesLiveTunnelService(t *testing.T) {
	f := newFixture(t)
	l := f.legacyState("old")
	lock := flock.New(filepath.Join(f.home, "tunnels", l.tunnelScope, "service.lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	f.asFrontend()
	if _, err := ExportMigration(f.home); !errors.Is(err, ErrBusy) {
		t.Fatalf("export: %v", err)
	}
}

func TestRollbackRestoresLegacyProfile(t *testing.T) {
	f := newFixture(t)
	l := f.legacyState("current")
	f.provision("current")
	receipt := f.migrate()
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath(), Receipt: &receipt}); err != nil {
		t.Fatal(err)
	}
	f.asBroker()
	bundle, err := Rollback(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenServer(f.layout); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("serve after rollback: %v", err)
	}
	f.asFrontend()
	if _, err := LoadMarker(f.home); err == nil {
		t.Fatal("stale activation accepted after rollback")
	}
	data, _ := json.Marshal(bundle)
	if err := RestoreLegacy(f.home, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// Repeating a restore is idempotent; a differing file is never replaced.
	if err := RestoreLegacy(f.home, bytes.NewReader(data)); err != nil {
		t.Fatalf("repeated restore: %v", err)
	}
	original, _ := os.ReadFile(filepath.Join(f.home, "admin.json"))
	f.write("admin.json", []byte(`{"gateway":"https://gw.example","key":"edited"}`))
	if err := RestoreLegacy(f.home, bytes.NewReader(data)); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("restore over a changed file: %v", err)
	}
	f.write("admin.json", original)
	if _, err := LoadMarker(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy mode after restore: %v", err)
	}
	var admin adminFile
	raw, _ := os.ReadFile(filepath.Join(f.home, "admin.json"))
	if json.Unmarshal(raw, &admin) != nil || admin.Key != "current" {
		t.Fatalf("restored admin = %s", raw)
	}
	if owner, _ := os.ReadFile(filepath.Join(f.home, "clients", l.ownerScope, "identity.key")); !bytes.Equal(owner, l.owner) {
		t.Fatal("owner key not restored")
	}
	var service client.StandaloneConfig
	raw, _ = os.ReadFile(filepath.Join(f.home, "tunnels", l.tunnelScope, "service.json"))
	if json.Unmarshal(raw, &service) != nil || service.StateDir != filepath.Join(f.home, "clients") || tunnelScope(service) != l.tunnelScope {
		t.Fatalf("restored tunnel login = %s", raw)
	}
	if got, _ := os.ReadFile(filepath.Join(f.home, "tunnels", l.tunnelScope, "tunnels.json")); !bytes.Equal(got, l.tunnels) {
		t.Fatal("tunnels not restored")
	}
	f.asBroker()
	if err := Purge(f.layout); err != nil {
		t.Fatal(err)
	}
	f.provision("next")
}

func TestActivateRefusesCredentialEnvironment(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	f.asFrontend()
	t.Setenv("CONTROL_USER_KEY", "k")
	if _, err := Activate(f.home, ActivateOptions{Source: f.layout.MarkerPath()}); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("activate: %v", err)
	}
}

func TestCheckServiceFileRequiresRoot(t *testing.T) {
	f := newFixture(t)
	unit := filepath.Join(f.base, "vault.service")
	if err := os.WriteFile(unit, []byte("[Service]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckServiceFile(unit); err == nil {
		t.Fatal("service definition owned by an ordinary account accepted")
	}
}

// snapshot records every entry below root so a read-only check can be proven
// to leave no trace.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = info.Mode().String() + info.ModTime().String() + strconv.FormatInt(info.Size(), 10)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestCheckProvisionIsReadOnly(t *testing.T) {
	f := newFixture(t)
	before := snapshot(t, f.base)
	if err := CheckProvision(f.layout, "everyday"); err != nil {
		t.Fatal(err)
	}
	for name, setup := range map[string]func(){
		"frontend is the broker": func() {
			hooks.lookup = func(string) (Principal, error) { return Principal{UID: strconv.Itoa(f.broker), Name: "self"}, nil }
		},
		"frontend is root": func() {
			hooks.lookup = func(string) (Principal, error) { return Principal{UID: "0", Name: "root"}, nil }
		},
		"unknown frontend": func() {
			hooks.lookup = func(name string) (Principal, error) { return Principal{}, errors.New("unknown " + name) }
		},
		"administrative process": func() { hooks.checkProcess = func() error { return errors.New("admin group") } },
		"untrusted binary":       func() { hooks.checkExecutable = func(int) error { return errors.New("writable binary") } },
		"root owned by another":  func() { hooks.uid = func() int { return f.broker + 1 } },
	} {
		saved := hooks
		setup()
		if err := CheckProvision(f.layout, "everyday"); err == nil {
			t.Errorf("%s accepted", name)
		}
		hooks = saved
	}
	if err := CheckProvision(Layout{Root: filepath.Join(f.base, "missing")}, "everyday"); err == nil {
		t.Error("missing root accepted")
	}
	if !sameSnapshot(before, snapshot(t, f.base)) {
		t.Fatal("preflight changed the filesystem")
	}

	f.provision("fresh")
	provisioned := snapshot(t, f.base)
	if err := CheckProvision(f.layout, "everyday"); !errors.Is(err, ErrExists) {
		t.Fatalf("provisioned: %v", err)
	}
	s, err := OpenServer(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	running := snapshot(t, f.base)
	if err := CheckProvision(f.layout, "everyday"); !errors.Is(err, ErrBusy) {
		t.Fatalf("serving: %v", err)
	}
	if !sameSnapshot(running, snapshot(t, f.base)) {
		t.Fatal("preflight changed a served profile")
	}
	_ = s.Close()
	if err := CheckProvision(f.layout, "everyday"); !errors.Is(err, ErrExists) {
		t.Fatalf("after serving: %v", err)
	}
	if !sameSnapshot(provisioned, snapshot(t, f.base)) {
		t.Fatal("preflight changed a stopped profile")
	}
}

func TestImportRejectsInexactTunnelExpiry(t *testing.T) {
	f := newFixture(t)
	l := f.legacyState("current")
	var infos []client.PersistentTunnelInfo
	if err := json.Unmarshal(l.tunnels, &infos); err != nil {
		t.Fatal(err)
	}
	infos[0].TTLSeconds = 60
	infos[0].Expires = infos[0].Created.Add(61 * time.Second)
	data, _ := json.Marshal(infos)
	f.write("tunnels/"+l.tunnelScope+"/tunnels.json", data)
	f.provision("current")
	f.asFrontend()
	bundle, err := ExportMigration(f.home)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(bundle)
	f.asBroker()
	if _, err := Import(f.layout, bytes.NewReader(raw)); err == nil {
		t.Fatal("inexact tunnel expiry accepted")
	}
	if exists(filepath.Join(f.layout.Root, "private", "tunnels", l.tunnelScope, "tunnels.json")) {
		t.Fatal("rejected import wrote tunnels")
	}
}

func TestPrivilegedGroups(t *testing.T) {
	names := map[int]string{0: "wheel", 20: "staff", 80: "admin", 999: "docker", 1000: "users"}
	got := privilegedGroups([]int{20, 1000, 80, 999, 0, 80}, func(gid int) string { return names[gid] })
	if len(got) != 3 {
		t.Fatalf("privileged = %v", got)
	}
	if got := privilegedGroups([]int{20, 1000}, func(gid int) string { return names[gid] }); len(got) != 0 {
		t.Fatalf("unprivileged = %v", got)
	}
}

func TestProvisionRejectsCallerStateDir(t *testing.T) {
	f := newFixture(t)
	_, err := Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}, StateDir: f.home},
		ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)})
	if err == nil {
		t.Fatal("caller state directory accepted")
	}
	if _, err := Provision(f.layout, ProvisionRequest{Address: "0.0.0.0:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}},
		ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)}); err == nil {
		t.Fatal("non-loopback address accepted")
	}
	hooks.lookup = func(string) (Principal, error) { return Principal{UID: strconv.Itoa(f.broker), Name: "self"}, nil }
	if _, err := Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:7443", Frontend: "self",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}},
		ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)}); err == nil {
		t.Fatal("frontend equal to broker accepted")
	}
}
