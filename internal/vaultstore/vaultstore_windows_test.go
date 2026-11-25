//go:build windows

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
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"golang.org/x/sys/windows"
)

// The test process plays both accounts. Kernel-reported owners and DACLs
// see its real SID as the broker; hooks present a second, fictitious
// frontend SID. The fixed root is replaced by a temporary directory whose
// descriptor stands in for the administrator's setup step.

const fakeDomain = `TEST\`

type fixture struct {
	t        *testing.T
	base     string
	layout   Layout
	home     string
	broker   string
	frontend string
}

func realSID(t *testing.T) string {
	t.Helper()
	sid, err := processSID()
	if err != nil {
		t.Fatal(err)
	}
	if !brokerSID(sid) {
		t.Skipf("ownership tests need an ordinary local or domain user account, not %s", sid)
	}
	return sid
}

// otherSID returns a syntactically valid SID in the same domain that no
// object in the test is owned by.
func otherSID(sid string, delta uint64) string {
	parts := strings.Split(sid, "-")
	rid, _ := strconv.ParseUint(parts[7], 10, 32)
	parts[7] = strconv.FormatUint((rid+delta)%(1<<32-1000)+1000, 10)
	return strings.Join(parts, "-")
}

func makeDir(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	sa := &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	return windows.CreateDirectory(p, sa)
}

// mustMakeDir creates a directory with an explicit descriptor. An
// administrator-owned descriptor needs an elevated test process.
func mustMakeDir(t *testing.T, path, sddl string) {
	t.Helper()
	if err := makeDir(path, sddl); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_OWNER) {
			t.Skip("standing in for the administrator setup step needs an elevated test process")
		}
		t.Fatal(err)
	}
}

func rootSDDL(broker string) string {
	return "O:BAD:P(A;;FA;;;BA)(A;;FA;;;SY)(A;OICI;FA;;;" + broker + ")(A;;0x1200a9;;;AU)"
}

func newFixtureWithRoot(t *testing.T, sddl func(broker string) string) *fixture {
	t.Helper()
	broker := realSID(t)
	base := t.TempDir()
	f := &fixture{t: t, base: base, layout: Layout{Root: filepath.Join(base, "Control Vault")}, home: filepath.Join(base, "home"), broker: broker, frontend: otherSID(broker, 4242)}
	mustMakeDir(t, f.layout.Root, sddl(broker))
	if err := os.Mkdir(f.home, 0o700); err != nil {
		t.Fatal(err)
	}
	saved := hooks
	t.Cleanup(func() { hooks = saved })
	realLookup := saved.lookupSID
	hooks.checkProcess = func() error { return nil }
	hooks.checkExecutable = func(string) error { return nil }
	hooks.defaultLayout = func() (Layout, error) { return f.layout, nil }
	hooks.lookup = func(name string) (Principal, error) {
		return Principal{UID: f.frontend, Name: fakeDomain + name}, nil
	}
	hooks.lookupSID = func(sid string) (Principal, error) {
		if sid == f.frontend {
			return Principal{UID: sid, Name: fakeDomain + "everyday"}, nil
		}
		return realLookup(sid)
	}
	f.asBroker()
	return f
}

func newFixture(t *testing.T) *fixture { return newFixtureWithRoot(t, rootSDDL) }

func (f *fixture) asBroker()   { hooks.sid = func() (string, error) { return f.broker, nil } }
func (f *fixture) asFrontend() { hooks.sid = func() (string, error) { return f.frontend, nil } }

func (f *fixture) provision(key string) Marker {
	f.t.Helper()
	f.asBroker()
	m, err := f.tryProvision(key)
	if err != nil {
		f.t.Fatal(err)
	}
	return m
}

func (f *fixture) tryProvision(key string) (Marker, error) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		f.t.Fatal(err)
	}
	return Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: key}},
		ApprovalKey: pub, Salt: bytes.Repeat([]byte{7}, 16)})
}

func (f *fixture) write(rel string, data []byte) {
	f.t.Helper()
	path := filepath.Join(f.home, filepath.FromSlash(rel))
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

// setDACL replaces an object's DACL with the one in sddl ("D:..." only).
func setDACL(t *testing.T, path, sddl string, protected bool) {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	info := windows.SECURITY_INFORMATION(windows.DACL_SECURITY_INFORMATION)
	if protected {
		info |= windows.PROTECTED_DACL_SECURITY_INFORMATION
	} else {
		info |= windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, info, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create a symbolic link: %v", err)
	}
}

func TestUserSID(t *testing.T) {
	for id, want := range map[string]bool{
		"S-1-5-21-1-2-3-1001":             true,
		"S-1-5-21-111-222-333-4294967295": true,
		"S-1-12-1-1-2-3-4":                true,
		"S-1-5-21-1-2-3-500":              false, // built-in Administrator
		"S-1-5-21-1-2-3-512":              false, // Domain Admins
		"S-1-5-18":                        false,
		"S-1-5-19":                        false,
		"S-1-5-32-544":                    false,
		"S-1-5-80-1-2-3-4-5":              false,
		"S-1-5-21-01-2-3-1001":            false,
		"s-1-5-21-1-2-3-1001":             false,
		"S-1-5-21-1-2-3":                  false,
		"1000":                            false,
		"":                                false,
	} {
		if got := userSID(id); got != want {
			t.Errorf("userSID(%q) = %v", id, got)
		}
		if got := validatePrincipal(Principal{UID: id, Name: "x"}) == nil; got != want {
			t.Errorf("validatePrincipal(%q) = %v", id, got)
		}
	}
	if brokerSID("S-1-12-1-1-2-3-4") {
		t.Error("cloud identity accepted as broker")
	}
}

func TestSplitAbs(t *testing.T) {
	for _, ok := range []string{`C:\`, `C:\a\b`, `\\?\D:\Program Files\x`, `c:\RUNNER~1\x`} {
		if _, _, err := splitAbs(ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{`C:a`, `a\b`, `\\server\share\x`, `C:\a\..\b`, `C:\a\b.`, `C:\a\b `, `C:\a:stream`, `C:\a\`, `C:/a`, `\\?\UNC\s\x`, `\\.\C:\a`} {
		if _, _, err := splitAbs(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func parse(t *testing.T, sddl string) *security {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseSecurity(sd, "test")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSecurityRules(t *testing.T) {
	const b = "S-1-5-21-1-2-3-1001"
	admins := trustedFor("")
	if err := parse(t, "O:SYD:(A;;FA;;;SY)(A;;0x1200a9;;;BU)(D;;FA;;;WD)").grants("x", admins, readRights, false); err != nil {
		t.Error(err)
	}
	if err := parse(t, "O:SYD:(A;;FW;;;BU)").grants("x", admins, readRights, false); err == nil {
		t.Error("user write accepted")
	}
	// ProgramData's own entry for Users: add entries, but not delete or relabel.
	sticky := parse(t, "O:SYD:(A;;0x116;;;BU)")
	if err := sticky.grants("x", admins, readRights|stickyRights, false); err != nil {
		t.Error(err)
	}
	if err := sticky.grants("x", admins, readRights, false); err == nil {
		t.Error("sticky rights accepted on a strict component")
	}
	for _, mask := range []string{"0x10000", "0x40", "0x40000", "0x80000", "GA", "GW"} {
		if err := parse(t, "O:SYD:(A;;"+mask+";;;BU)").grants("x", admins, readRights|stickyRights, false); err == nil {
			t.Errorf("mask %s accepted on an ancestor", mask)
		}
	}
	inherit := parse(t, "O:SYD:(A;OICIIO;FA;;;BU)")
	if err := inherit.grants("x", admins, 0, false); err != nil {
		t.Error(err)
	}
	if err := inherit.grants("x", admins, 0, true); err == nil {
		t.Error("inheritable grant accepted on a private object")
	}
	if err := parse(t, "O:"+b+"D:P(A;OICI;FA;;;"+b+")").grants("x", trustedFor(b), 0, true); err != nil {
		t.Error(err)
	}
	for _, sddl := range []string{"O:SYD:NO_ACCESS_CONTROL", "O:SY"} {
		sd, err := windows.SecurityDescriptorFromString(sddl)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseSecurity(sd, "x"); err == nil {
			t.Errorf("%s accepted", sddl)
		}
	}
}

func TestRootPolicy(t *testing.T) {
	const b = "S-1-5-21-1-2-3-1001"
	const c = "S-1-5-21-1-2-3-1002"
	if got, err := rootBroker(parse(t, "O:BAD:P(A;;FA;;;BA)(A;;FA;;;SY)(A;OICI;FA;;;"+b+")(A;;FR;;;AU)"), "r", ""); err != nil || got != b {
		t.Fatalf("broker = %q, %v", got, err)
	}
	if _, err := rootBroker(parse(t, "O:BAD:P(A;;FA;;;"+b+")"), "r", c); err == nil {
		t.Error("broker differing from the chain account accepted")
	}
	if _, err := rootBroker(parse(t, "O:"+b+"D:P(A;;FA;;;"+b+")"), "r", ""); !errors.Is(err, errNotAdministered) {
		t.Errorf("user-owned root: %v", err)
	}
	for name, sddl := range map[string]string{
		"inherited":     "O:BAD:(A;;FA;;;" + b + ")",
		"two writers":   "O:BAD:P(A;;FA;;;" + b + ")(A;;FW;;;" + c + ")",
		"users write":   "O:BAD:P(A;;FA;;;" + b + ")(A;;0x2;;;BU)",
		"service":       "O:BAD:P(A;;FA;;;LS)",
		"read only":     "O:BAD:P(A;;FA;;;BA)(A;;FR;;;AU)",
		"cloud account": "O:BAD:P(A;;FA;;;S-1-12-1-1-2-3-4)",
	} {
		if _, err := rootBroker(parse(t, sddl), "r", ""); err == nil {
			t.Errorf("%s root accepted", name)
		}
	}
}

func TestBrokerTokenPolicy(t *testing.T) {
	ordinary := func() *tokenFacts {
		return &tokenFacts{user: "S-1-5-21-1-2-3-1001", groups: []string{"S-1-1-0", "S-1-5-32-545", "S-1-5-11"},
			privileges: []string{"SeChangeNotifyPrivilege", "SeIncreaseWorkingSetPrivilege"}, integrity: 0x2000}
	}
	if p := ordinary().brokerProblems(nil); len(p) != 0 {
		t.Fatalf("ordinary token refused: %v", p)
	}
	for name, mutate := range map[string]func(*tokenFacts){
		"system":         func(f *tokenFacts) { f.user = "S-1-5-18" },
		"local service":  func(f *tokenFacts) { f.user = "S-1-5-19" },
		"administrator":  func(f *tokenFacts) { f.user = "S-1-5-21-1-2-3-500" },
		"elevated":       func(f *tokenFacts) { f.elevated = true },
		"high integrity": func(f *tokenFacts) { f.integrity = 0x3000 },
		"admins":         func(f *tokenFacts) { f.groups = append(f.groups, "S-1-5-32-544") },
		"backup":         func(f *tokenFacts) { f.groups = append(f.groups, "S-1-5-32-551") },
		"domain admins":  func(f *tokenFacts) { f.groups = append(f.groups, "S-1-5-21-9-9-9-512") },
		"docker":         func(f *tokenFacts) { f.groups = append(f.groups, "S-1-5-21-1-2-3-1010") },
		"impersonate":    func(f *tokenFacts) { f.privileges = append(f.privileges, "SeImpersonatePrivilege") },
		"debug":          func(f *tokenFacts) { f.privileges = append(f.privileges, "SeDebugPrivilege") },
		"virtualized":    func(f *tokenFacts) { f.virtualized = true },
		"appcontainer":   func(f *tokenFacts) { f.container = true },
	} {
		f := ordinary()
		mutate(f)
		if p := f.brokerProblems(map[string]bool{"S-1-5-21-1-2-3-1010": true}); len(p) == 0 {
			t.Errorf("%s token accepted", name)
		}
	}
}

func TestBrokerProcessRefusesAdministrators(t *testing.T) {
	f, err := currentToken()
	if err != nil {
		t.Fatal(err)
	}
	if !f.elevated && len(privilegedGroups(f.groups, nil)) == 0 {
		t.Skip("the test account is not an administrator")
	}
	if err := checkBrokerProcess(); err == nil {
		t.Fatal("administrator token accepted as broker")
	}
	if warnings := FrontendWarnings(); len(warnings) == 0 {
		t.Fatal("no warning for an administrative everyday account")
	}
}

func TestCanonicalLayout(t *testing.T) {
	layout, err := canonicalLayout()
	if err != nil {
		t.Fatal(err)
	}
	win, err := windows.GetSystemWindowsDirectory()
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.ToUpper(filepath.VolumeName(win)) + `\ProgramData\Control Vault`; layout.Root != want {
		t.Fatalf("root = %s, want %s", layout.Root, want)
	}
	if _, err := requireCanonical(`C:\Temp\Control Vault`); err == nil {
		t.Fatal("non-canonical root accepted")
	}
	if _, err := requireCanonical(strings.ToLower(layout.Root)); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionServeAndActivate(t *testing.T) {
	f := newFixture(t)
	if err := CheckProvision(f.layout, "everyday"); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.layout.Root, "private")) {
		t.Fatal("preflight created the private directory")
	}
	m := f.provision("fresh")
	if m.Pin != Pin(m.Certificate) || m.Broker.UID != f.broker || m.Frontend.UID != f.frontend {
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
	if err := CheckProvision(f.layout, "everyday"); !errors.Is(err, ErrBusy) {
		t.Fatalf("preflight while serving: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckProvision(f.layout, "everyday"); !errors.Is(err, ErrExists) {
		t.Fatalf("preflight after provisioning: %v", err)
	}
	if _, err := f.tryProvision("again"); !errors.Is(err, ErrExists) {
		t.Fatalf("reprovision: %v", err)
	}
	// The private directory and its files are protected and broker-only.
	for _, rel := range []string{"private", `private\profile.json`, `private\tls.key`, `private\clients`} {
		sd, err := windows.GetNamedSecurityInfo(filepath.Join(f.layout.Root, rel), windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
		if err != nil {
			t.Fatal(err)
		}
		s, err := parseSecurity(sd, rel)
		if err != nil {
			t.Fatal(err)
		}
		if s.owner != f.broker || !s.protected {
			t.Fatalf("%s owner %s protected %v", rel, s.owner, s.protected)
		}
		for _, a := range s.aces {
			if a.sid != f.broker {
				t.Fatalf("%s grants %s", rel, a.sid)
			}
		}
	}
	published, err := os.ReadFile(f.layout.MarkerPath())
	if err != nil || bytes.Contains(published, []byte("fresh")) {
		t.Fatalf("marker %s: %v", published, err)
	}

	f.asFrontend()
	if _, err := LoadMarker(f.home); !errors.Is(err, ErrActivationMissing) {
		t.Fatalf("before activation: %v", err)
	}
	local, err := Activate(f.home, ActivateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadMarker(f.home)
	if err != nil || loaded.Pin != m.Pin || !strings.EqualFold(loaded.Source, f.layout.MarkerPath()) {
		t.Fatalf("load = %+v, %v", loaded, err)
	}
	if err := CheckAmbient(f.home); err != nil {
		t.Fatal(err)
	}
	// A lookalike written by the frontend into its own copy is rejected.
	local.Address = "127.0.0.1:9999"
	data, _ := json.Marshal(local)
	f.write(MarkerName, data)
	if _, err := LoadMarker(f.home); err == nil {
		t.Fatal("tampered activation accepted")
	}
	// A manual source outside the fixed root is never trusted.
	if _, err := Activate(f.home, ActivateOptions{Source: filepath.Join(f.home, MarkerName)}); err == nil {
		t.Fatal("marker outside the fixed root accepted")
	}
}

func TestPreflightRefusesSameAccount(t *testing.T) {
	f := newFixture(t)
	hooks.lookup = func(string) (Principal, error) { return Principal{UID: f.broker, Name: fakeDomain + "self"}, nil }
	if err := CheckProvision(f.layout, "self"); err == nil {
		t.Fatal("frontend equal to broker accepted")
	}
	if err := CheckProvision(Layout{Root: filepath.Join(f.base, "elsewhere")}, "everyday"); err == nil {
		t.Fatal("non-canonical layout accepted")
	}
	hooks.sid = func() (string, error) { return otherSID(f.broker, 7), nil }
	if err := CheckProvision(f.layout, "everyday"); err == nil {
		t.Fatal("root assigned to another account accepted")
	}
}

func TestMarkerAssignedToFrontendIsRejected(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	// The caller is the account the root is assigned to: anything the
	// frontend can write must never become a trusted marker.
	hooks.sid = func() (string, error) { return f.broker, nil }
	if _, err := Activate(f.home, ActivateOptions{}); err == nil {
		t.Fatal("self-assigned vault accepted")
	}
}

func TestRootMustBeAdministered(t *testing.T) {
	// Any account may create this directory in ProgramData; one it owns
	// proves nothing and must not block or capture the everyday account.
	f := newFixtureWithRoot(t, func(b string) string {
		return "O:" + b + "D:P(A;OICI;FA;;;" + b + ")(A;;0x1200a9;;;AU)"
	})
	if _, err := f.tryProvision("fresh"); !errors.Is(err, errNotAdministered) {
		t.Fatalf("provision in a user-owned root: %v", err)
	}
	// Even a well-formed marker for this frontend planted there is ignored.
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := certificate("127.0.0.1:7443", pub, key)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := principalFor(f.broker)
	if err != nil {
		t.Fatal(err)
	}
	planted := Marker{Version: markerVersion, Address: "127.0.0.1:7443", Pin: Pin(der), Certificate: der, Salt: bytes.Repeat([]byte{1}, 16),
		Broker: owner, Frontend: Principal{UID: f.frontend, Name: fakeDomain + "everyday"}, Gateway: "https://gw.example", Created: time.Now().UTC().Truncate(time.Second)}
	if err := planted.Validate(); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(planted)
	if err := os.WriteFile(f.layout.MarkerPath(), data, 0o644); err != nil {
		t.Fatal(err)
	}
	f.asFrontend()
	if _, err := LoadMarker(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy mode with a user-owned root: %v", err)
	}
	if _, err := Activate(f.home, ActivateOptions{}); err == nil {
		t.Fatal("activation against a user-owned root")
	}
}

func TestRootDACLIsEnforced(t *testing.T) {
	for name, sddl := range map[string]func(string) string{
		"users may write": func(b string) string { return rootSDDL(b) + "(A;;0x2;;;BU)" },
		"inherits":        func(b string) string { return "O:BAD:(A;OICI;FA;;;" + b + ")(A;;0x1200a9;;;AU)" },
		"second writer": func(b string) string {
			return rootSDDL(b) + "(A;;FA;;;" + otherSID(b, 99) + ")"
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixtureWithRoot(t, sddl)
			if _, err := f.tryProvision("fresh"); err == nil {
				t.Fatal("root accepted")
			}
		})
	}
	t.Run("changed after provisioning", func(t *testing.T) {
		f := newFixture(t)
		f.provision("fresh")
		setDACL(t, f.layout.Root, "D:(A;;FA;;;BA)(A;OICI;FA;;;"+f.broker+")(A;;0x1200a9;;;AU)(A;;0x6;;;BU)", true)
		if _, err := OpenServer(f.layout); err == nil {
			t.Fatal("writable root accepted by the broker")
		}
		f.asFrontend()
		if _, err := Activate(f.home, ActivateOptions{}); err == nil {
			t.Fatal("writable root accepted by the frontend")
		}
		if _, err := LoadMarker(f.home); !errors.Is(err, ErrUnverified) {
			t.Fatalf("legacy decision with a broken administered root: %v", err)
		}
	})
}

func TestTrustedWalkRejectsSubstitution(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	private := filepath.Join(f.layout.Root, "private")

	// A link anywhere on the walk is refused, even to the real root.
	link := filepath.Join(f.base, "link")
	symlinkOrSkip(t, f.base, link)
	viaLink := Layout{Root: filepath.Join(link, "Control Vault")}
	hooks.defaultLayout = func() (Layout, error) { return viaLink, nil }
	if _, err := OpenServer(viaLink); err == nil {
		t.Fatal("root through a link accepted")
	}
	f.asFrontend()
	if _, err := Activate(f.home, ActivateOptions{}); err == nil {
		t.Fatal("marker through a link accepted")
	}
	f.asBroker()
	hooks.defaultLayout = func() (Layout, error) { return f.layout, nil }

	planted := filepath.Join(private, "clients", "planted")
	symlinkOrSkip(t, f.home, planted)
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("link inside the private profile accepted")
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
	profile := filepath.Join(private, "profile.json")
	setDACL(t, profile, "D:(A;;FA;;;"+f.broker+")(A;;FR;;;AU)", true)
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("readable profile accepted")
	}
	setDACL(t, profile, "D:(A;;FA;;;"+f.broker+")", true)
	setDACL(t, private, "D:(A;OICI;FA;;;"+f.broker+")", false)
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("private directory inheriting from the root accepted")
	}
	setDACL(t, private, "D:(A;OICI;FA;;;"+f.broker+")", true)
	s, err := OpenServer(f.layout)
	if err != nil {
		t.Fatalf("restored profile: %v", err)
	}
	_ = s.Close()
	// Only the account the root is assigned to may run the broker.
	hooks.sid = func() (string, error) { return otherSID(f.broker, 1), nil }
	if _, err := OpenServer(f.layout); err == nil {
		t.Fatal("broker for another account accepted")
	}
}

func TestOpenTrustedRules(t *testing.T) {
	broker := realSID(t)
	base := t.TempDir()
	writable := filepath.Join(base, "writable")
	mustMakeDir(t, writable, "O:"+broker+"D:P(A;OICI;FA;;;"+broker+")(A;;FA;;;BU)")
	if d, err := openTrusted(writable, broker, true, dirRead); err == nil {
		_ = d.Close()
		t.Fatal("user-writable directory accepted")
	}
	readable := filepath.Join(base, "readable")
	mustMakeDir(t, readable, "O:"+broker+"D:P(A;OICI;FA;;;"+broker+")(A;;0x1200a9;;;BU)")
	d, err := openTrusted(readable, broker, true, dirRead)
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if d, err := openTrusted(readable, otherSID(broker, 3), true, dirRead); err == nil {
		_ = d.Close()
		t.Fatal("directory owned by another account accepted")
	}
	sticky := filepath.Join(base, "sticky")
	mustMakeDir(t, sticky, "O:BAD:P(A;OICI;FA;;;BA)(A;;0x116;;;BU)(A;;0x1200a9;;;BU)")
	if err := os.Mkdir(filepath.Join(sticky, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if d, err := openTrusted(filepath.Join(sticky, "child"), broker, true, dirRead); err != nil {
		t.Fatalf("administrator-owned sticky ancestor refused: %v", err)
	} else {
		_ = d.Close()
	}
	if d, err := openTrusted(sticky, broker, true, dirRead); err == nil {
		_ = d.Close()
		t.Fatal("sticky rights accepted on the final component")
	}
}

func TestCheckExecutable(t *testing.T) {
	broker := realSID(t)
	// The test binary lives in this account's own temporary directory.
	if err := checkExecutable(broker); err != nil {
		t.Fatalf("test binary refused: %v", err)
	}
}

func TestActivateRefusesPlaintextAndNodeTokens(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	f.asFrontend()
	if _, err := ExportMigration(f.home); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("export: %v", err)
	}
	f.asBroker()
	if _, err := Import(f.layout, bytes.NewReader(nil)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("import: %v", err)
	}
	f.asFrontend()
	if _, err := Activate(f.home, ActivateOptions{Receipt: &Receipt{}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("receipt: %v", err)
	}

	scope := identity.ID([]byte("https://gw.example\x00user"))
	for _, rel := range []string{"admin.json", "update-admin.json", "clients/" + scope + "/identity.key", "tunnels/" + scope + "/service.json", "tunnels/" + scope + "/endpoint.json"} {
		t.Run(rel, func(t *testing.T) {
			path := filepath.Join(f.home, filepath.FromSlash(rel))
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(`{"gateway":"https://gw.example","key":"old","token":"t"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = os.Remove(path) }()
			_, err := Activate(f.home, ActivateOptions{})
			if !errors.Is(err, ErrAmbientCredentials) || !errors.Is(err, ErrUnsupported) {
				t.Fatalf("activate: %v", err)
			}
			if !exists(path) || exists(filepath.Join(f.home, MarkerName)) {
				t.Fatal("refused activation changed the home")
			}
		})
	}
	f.write("node.json", []byte(`{"name":"n","gateway":"https://gw.example","token":"node"}`))
	if _, err := Activate(f.home, ActivateOptions{}); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("node token: %v", err)
	}
	if err := os.Remove(filepath.Join(f.home, "node.json")); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(f.base, "custom-node.json")
	if err := os.WriteFile(extra, []byte(`{"token":"node"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(f.home, ActivateOptions{NodeConfigs: []string{extra}}); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("selected node token: %v", err)
	}
	t.Setenv("CONTROL_TOKEN", "t")
	if _, err := Activate(f.home, ActivateOptions{}); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("token environment: %v", err)
	}
	t.Setenv("CONTROL_TOKEN", "")
	if _, err := Activate(f.home, ActivateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Credentials reappearing after activation stop vault mode.
	f.write("admin.json", []byte(`{"gateway":"https://gw.example","key":"again"}`))
	if _, err := LoadMarker(f.home); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("ambient after activation: %v", err)
	}
	if err := os.Remove(filepath.Join(f.home, "admin.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home, extra); !errors.Is(err, ErrAmbientCredentials) {
		t.Fatalf("selected node token after activation: %v", err)
	}
}

func TestActivateRefusesLiveTunnelService(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	dir := filepath.Join(f.home, "tunnels", identity.ID([]byte("scope")))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, "service.lock"))
	if ok, err := lock.TryLock(); err != nil || !ok {
		t.Fatal(err)
	}
	f.asFrontend()
	if _, err := Activate(f.home, ActivateOptions{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("live tunnel service: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Activate(f.home, ActivateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackReleasesNoCredentials(t *testing.T) {
	f := newFixture(t)
	f.provision("fresh")
	f.asFrontend()
	if _, err := Activate(f.home, ActivateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.asBroker()
	bundle, err := Rollback(f.layout)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(bundle)
	if len(bundle.Items) != 0 || bytes.Contains(data, []byte("fresh")) {
		t.Fatalf("rollback bundle released credentials: %s", data)
	}
	if _, err := OpenServer(f.layout); !errors.Is(err, ErrRolledBack) {
		t.Fatalf("serve after rollback: %v", err)
	}
	f.asFrontend()
	if _, err := LoadMarker(f.home); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale activation after rollback: %v", err)
	}
	forged := bundle
	forged.Items = []BundleItem{{Kind: ItemAccount, Path: "admin.json", Digest: digest([]byte("{}")), Data: []byte("{}")}}
	forgedData, _ := json.Marshal(forged)
	if err := RestoreLegacy(f.home, bytes.NewReader(forgedData)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("restore with credentials: %v", err)
	}
	if err := RestoreLegacy(f.home, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMarker(f.home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy mode after restore: %v", err)
	}
	f.asBroker()
	if err := Purge(f.layout); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(f.layout.Root, "private", "tls.key")) || exists(filepath.Join(f.layout.Root, "private", "clients")) {
		t.Fatal("purge left private state")
	}
	f.provision("next")
}

func TestProvisionRejectsCallerStateDir(t *testing.T) {
	f := newFixture(t)
	if _, err := Provision(f.layout, ProvisionRequest{Address: "127.0.0.1:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}, StateDir: f.home},
		ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)}); err == nil {
		t.Fatal("caller state directory accepted")
	}
	if _, err := Provision(f.layout, ProvisionRequest{Address: "0.0.0.0:7443", Frontend: "everyday",
		Account:     client.StandaloneConfig{Gateway: client.Admin{URL: "https://gw.example", Key: "k"}},
		ApprovalKey: make([]byte, 32), Salt: make([]byte, 16)}); err == nil {
		t.Fatal("non-loopback address accepted")
	}
}
