package node

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

type proxySourceListener struct{ net.Listener }

func (l proxySourceListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return proxySourceConn{c}, nil
}

type proxySourceConn struct{ net.Conn }

func (c proxySourceConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("10.0.0.8"), Port: 12345}
}

func TestProxySameAddressReconfigurationNarrowsRunningListener(t *testing.T) {
	m, err := newManagedProxy(Config{Gateway: "https://gateway.example", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	m.listen = func(context.Context, string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		return proxySourceListener{ln}, nil
	}
	m.start(t.Context())
	t.Cleanup(m.closeAll)
	q := model.ProxyConfigure{Enabled: true, Interface: "nic", Address: "10.0.0.5", Port: 8080, Sources: []string{"10.0.0.0/24"}}
	interfaces := []model.ProxyInterface{{Name: "nic", Address: q.Address}}
	if err := m.configure(q, interfaces); err != nil {
		t.Fatal(err)
	}
	endpoint, _ := q.Endpoint()
	listener := m.listeners[endpoint.URL]
	stale := m.endpoints[endpoint.URL]
	c, _, status := connectThrough(t, listener.Addr().String(), "GET / HTTP/1.1\r\nHost: gateway.example\r\n\r\n")
	_ = c.Close()
	if status != "HTTP/1.1 405 Method Not Allowed\r\n" {
		t.Fatalf("initial allowed source was not admitted: %q", status)
	}
	eventuallyTrue(t, func() bool { return !m.busy() })
	q.Sources = []string{"10.0.0.9/32"}
	if err := m.configure(q, interfaces); err != nil {
		t.Fatal(err)
	}
	if m.listeners[endpoint.URL] != listener {
		t.Fatal("same-address change replaced the listening socket")
	}
	c, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if count, err := c.Read(make([]byte, 1)); count != 0 || err == nil {
		t.Fatal("formerly allowed source was not rejected")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("source remained admitted with the stale allowlist")
	}
	if err := m.configure(model.ProxyConfigure{}, interfaces); err != nil {
		t.Fatal(err)
	}
	server, client := net.Pipe()
	defer func() { _ = server.Close(); _ = client.Close() }()
	if m.admit(proxySourceConn{server}, stale) {
		t.Fatal("removed endpoint admitted a socket from its old accept loop")
	}
}

func TestProxyCheckRetriesDurableEnabledListenerAfterBindFailure(t *testing.T) {
	m, _, _ := managedFixture(t, "127.0.0.0/8")
	// Model restart from the existing desired setting with a transient bind
	// failure. Retry must neither persist a new decision nor reconfigure it.
	m.mu.Lock()
	_ = m.listeners[managedTestURL].Close()
	delete(m.listeners, managedTestURL)
	m.mu.Unlock()
	listen := m.listen
	attempts := 0
	m.listen = func(ctx context.Context, address string) (net.Listener, error) {
		attempts++
		if attempts == 1 {
			return nil, errors.New("temporarily unavailable")
		}
		return listen(ctx, address)
	}
	m.write = func(string, any) error { t.Fatal("retry persisted settings"); return nil }
	m.setSupported(true)
	checks := newProxyChecks([]model.ProxyEndpoint{{URL: managedTestURL, Managed: true}}, m)
	checks.probe = func(context.Context, string, string) error { return nil }
	checks.checkAll(t.Context(), "https://gateway.example")
	if _, listening, _ := m.endpointState(managedTestURL); listening {
		t.Fatal("failed bind reported listening")
	}
	checks.checkAll(t.Context(), "https://gateway.example")
	if _, listening, failure := m.endpointState(managedTestURL); !listening || failure != "" {
		t.Fatal("check cadence did not restore desired listener")
	}
	if got := checks.snapshot(time.Now()); len(got) != 1 || got[0].State != model.ProxyReady {
		t.Fatalf("recovered listener not checked ready: %+v", got)
	}
	if err := m.apply(nil); err != nil {
		t.Fatal(err)
	}
	checks.checkAll(t.Context(), "https://gateway.example")
	if _, listening, _ := m.endpointState(managedTestURL); listening || attempts != 2 {
		t.Fatal("periodic retry re-enabled a disabled listener")
	}
}

func TestSystemProxyManagementRequiresDestinationPermission(t *testing.T) {
	n := &Node{Config: Config{InstructionOnly: true, Allow: map[string][]string{"@orchestrator": {"proxy.manage"}}}, managedProxy: &managedProxy{}}
	if n.proxyManagementHandler() == nil {
		t.Fatal("explicit System target with proxy.manage did not advertise support")
	}
	n.Config.Allow = map[string][]string{"@orchestrator": {"secrets.manage"}}
	if n.proxyManagementHandler() != nil {
		t.Fatal("System target gained proxy management without destination permission")
	}
}

func TestProxyManagementExplicitLifecycleAndPersistence(t *testing.T) {
	cfg := Config{Gateway: "https://gateway.example", DataDir: t.TempDir(), ProxyEndpoints: []model.ProxyEndpoint{{URL: "http://external.example:8080"}}}
	m, err := newManagedProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.start(t.Context())
	t.Cleanup(m.closeAll)
	var sockets []net.Listener
	m.listen = func(_ context.Context, address string) (net.Listener, error) {
		if address != "10.20.0.5:8080" {
			t.Errorf("unexpected bind %s", address)
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		sockets = append(sockets, ln)
		return ln, err
	}
	interfaces := []model.ProxyInterface{{Name: "ethernet", Address: "10.20.0.5", Prefix: "10.20.0.0/24"}}
	q := model.ProxyConfigure{Enabled: true, Interface: "ethernet", Address: "10.20.0.5", Port: 8080, Sources: []string{"10.20.0.0/24"}}
	if err := m.configure(q, interfaces); err != nil {
		t.Fatal(err)
	}
	if !m.configuration(interfaces).Enabled || len(m.listeners) != 1 || len(m.advertisementLocked()) != 2 {
		t.Fatal("enable without dependencies did not bind/preserve external endpoint")
	}
	restored, err := newManagedProxy(cfg)
	if err != nil || !restored.enabled || len(restored.desired) != 1 || restored.settings.Interface != "ethernet" {
		t.Fatalf("restart lost applied settings: %v %+v", err, restored)
	}
	m.mu.Lock()
	m.active = 1
	m.mu.Unlock()
	if err := m.configure(model.ProxyConfigure{}, interfaces); err == nil {
		t.Fatal("disabled active CONNECT work")
	}
	m.mu.Lock()
	m.active = 0
	m.mu.Unlock()
	write := m.write
	m.write = func(string, any) error { return errors.New("disk full") }
	if err := m.configure(model.ProxyConfigure{}, interfaces); err == nil || len(m.listeners) != 1 || !m.enabled {
		t.Fatal("failed persistence changed listener")
	}
	m.write = write
	if err := m.configure(model.ProxyConfigure{}, interfaces); err == nil {
		t.Fatal("uncertain storage was silently retried")
	}
	// Recover by restoring the durable state in a fresh runtime.
	m.closeAll()
	m, err = newManagedProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	m.start(t.Context())
	t.Cleanup(m.closeAll)
	if err := m.configure(model.ProxyConfigure{}, interfaces); err != nil || len(m.listeners) != 0 {
		t.Fatalf("explicit disable: %v", err)
	}
	if _, err := sockets[0].Accept(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("disabled listening socket remains open: %v", err)
	}
	restored, err = newManagedProxy(cfg)
	if err != nil || restored.enabled || len(restored.desired) != 0 || len(restored.advertisementLocked()) != 1 {
		t.Fatalf("restart lost disable override: %v", err)
	}
}

func TestProxyManagementValidatesAssignedInterfaceAndSources(t *testing.T) {
	interfaces := []model.ProxyInterface{{Name: "ethernet", Address: "10.0.0.5", Prefix: "10.0.0.0/24"}, {Name: "vpn", Address: "fd00::5", Prefix: "fd00::/64"}}
	valid := model.ProxyConfigure{Enabled: true, Interface: "ethernet", Address: "10.0.0.5", Port: 8080, Sources: []string{"10.0.0.0/24"}}
	if err := validateProxyInterface(valid, interfaces); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"10.0.0.6", "127.0.0.1", "0.0.0.0", "169.254.1.2", "8.8.8.8", "::1", "fe80::5", "2001:4860::1"} {
		q := valid
		q.Address = address
		if err := validateProxyInterface(q, interfaces); err == nil {
			t.Errorf("accepted unassigned/unsafe address %s", address)
		}
	}
	q := valid
	q.Interface = "missing"
	if err := validateProxyInterface(q, interfaces); err == nil {
		t.Fatal("accepted wrong interface")
	}
	q = valid
	q.Interface, q.Address, q.Sources = "vpn", "fd00::5", []string{"fd00::/64"}
	if err := validateProxyInterface(q, interfaces); err != nil {
		t.Fatal(err)
	}
	for _, sources := range [][]string{nil, {"0.0.0.0/0"}, {"10.0.0.5"}, {"8.8.8.0/24"}, {"fe80::/64"}, make([]string, 17)} {
		q = valid
		q.Sources = sources
		if err := validateProxyInterface(q, interfaces); err == nil {
			t.Fatalf("accepted sources %v", sources)
		}
	}
}

func TestNativeProxyInterfacesComeFromLiveAssignedAddresses(t *testing.T) {
	interfaces, err := nativeProxyInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range interfaces {
		nic, err := net.InterfaceByName(entry.Name)
		if err != nil || nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 {
			t.Fatalf("ineligible native interface %+v: %v", entry, err)
		}
		prefix, err := netip.ParsePrefix(entry.Prefix)
		address, addrErr := netip.ParseAddr(entry.Address)
		if err != nil || addrErr != nil || prefix != prefix.Masked() || !prefix.Contains(address) {
			t.Fatalf("invalid canonical prefix %+v", entry)
		}
		addresses, err := nic.Addrs()
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, native := range addresses {
			p, _ := netip.ParsePrefix(native.String())
			found = found || p.Addr() == address
		}
		if !found {
			t.Fatalf("unassigned address %+v", entry)
		}
	}
}

func TestProxyManagementFailedBindKeepsDurableSettings(t *testing.T) {
	dataDir := t.TempDir()
	m, err := newManagedProxy(Config{Gateway: "https://gateway.example", DataDir: dataDir})
	if err != nil {
		t.Fatal(err)
	}
	m.start(t.Context())
	t.Cleanup(m.closeAll)
	m.listen = func(context.Context, string) (net.Listener, error) { return nil, errors.New("in use") }
	m.write = func(string, any) error { t.Fatal("persisted before binding"); return nil }
	q := model.ProxyConfigure{Enabled: true, Interface: "nic", Address: "10.0.0.5", Port: 8080, Sources: []string{"10.0.0.0/24"}}
	if err := m.configure(q, []model.ProxyInterface{{Name: "nic", Address: q.Address}}); err == nil || m.enabled {
		t.Fatal("failed bind reported applied")
	}
	if m.settingsPath != filepath.Join(dataDir, "proxy-settings.json") {
		t.Fatal("settings not stored under worker data directory")
	}
}
