package model

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Gateway proxy limits. A worker advertises a few explicitly configured
// endpoints, never every interface, and reports bounded readiness for each.
const (
	MaxProxyEndpoints      = 8
	MaxProxyInterfaceBytes = 64
	MaxProxyErrorBytes     = 160
	// ProxyCheckInterval is the healthy worker's proxy-to-gateway check cadence,
	// independent of dashboard polling.
	ProxyCheckInterval = time.Minute
	// ProxyReadinessTTL turns an older observation into "stale", which is
	// never eligible for new invitations.
	ProxyReadinessTTL = 3 * ProxyCheckInterval
	// MaxProxySources bounds a managed listener's source CIDR allowlist.
	MaxProxySources = 16
)

// Proxy readiness states. ProxyReady is eligible for invitation selection;
// ProxyStandby is eligible only for a Control-managed endpoint.
const (
	ProxyReady = "ready"
	// ProxyStandby reports a managed listener that is intentionally off because
	// nothing depends on it, after the worker freshly verified the direct
	// gateway path the listener would use. Invitation creation activates it.
	ProxyStandby     = "standby"
	ProxyUnavailable = "unavailable"
	ProxyStale       = "stale"
	ProxyOffline     = "offline"
	ProxyUnchecked   = "unchecked"
)

// ProxyEndpoint is a CONNECT proxy address a worker explicitly advertises for
// machines that cannot reach the gateway directly. Configuration, not readiness.
type ProxyEndpoint struct {
	URL string `json:"url"`
	// Interface optionally labels the LAN or VPN the address belongs to.
	Interface string `json:"interface,omitempty"`
	// Managed selects Control's own fixed-gateway CONNECT listener on URL's
	// private address instead of separately managed proxy software. The
	// owner explicitly enables or disables it; dependencies prevent unsafe
	// disable/reconfiguration. It never controls an external proxy.
	Managed bool `json:"managed,omitempty"`
	// Sources lists the private/VPN CIDRs a managed listener accepts. It is
	// local profile configuration and is not advertised in registration.
	Sources []string `json:"sources,omitempty"`
}

// ProxyReadiness is a worker's bounded proxy-to-gateway check of one endpoint.
// Ready proves CONNECT, gateway TLS verification, and a gateway health
// response through that proxy. It does not prove a future child can reach it.
type ProxyReadiness struct {
	URL       string    `json:"url"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checkedAt"`
	// AgeSeconds lets the gateway date the check on its own clock.
	AgeSeconds int    `json:"ageSeconds"`
	Error      string `json:"error,omitempty"`
}

// ProxyEndpointStatus is the gateway's fleet-scoped projection of an
// advertised endpoint and its latest readiness observation.
type ProxyEndpointStatus struct {
	URL       string    `json:"url"`
	Interface string    `json:"interface,omitempty"`
	Managed   bool      `json:"managed,omitempty"`
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
	// Dependents counts what prevents disabling a managed listener.
	Dependents *ProxyDependents `json:"dependents,omitempty"`
}

// Selectable reports whether a new invitation can select this endpoint now.
func (s ProxyEndpointStatus) Selectable() bool {
	return s.State == ProxyReady || (s.Managed && s.State == ProxyStandby)
}

// ProxyDependents counts a managed endpoint's durable gateway dependencies:
// pending usable invitations (including Hybrid companions), enrolled children
// whether online or not, and unregistered children that have not yet
// acknowledged their signed retirement policy.
type ProxyDependents struct {
	Invitations int `json:"invitations"`
	Children    int `json:"children"`
	Retiring    int `json:"retiring"`
	// Machines groups enrolled children by their gateway-issued Hybrid pair.
	// It is display-only and does not add to Total.
	Machines int `json:"machines"`
}

// Total is the number of dependencies preventing explicit disable.
func (d ProxyDependents) Total() int { return d.Invitations + d.Children + d.Retiring }

// ProxyStateProof lets a worker read its own desired managed listeners. It is
// signed by the worker identity and authorizes no mutation.
type ProxyStateProof struct {
	PublicKey []byte    `json:"publicKey"`
	SignedAt  time.Time `json:"signedAt"`
	Signature []byte    `json:"signature"`
}

func (p ProxyStateProof) Message() []byte {
	return JSON(struct {
		Purpose   string
		PublicKey []byte
		SignedAt  time.Time
	}{"control-proxy-state-v1", p.PublicKey, p.SignedAt})
}

// ProxyState lists the managed endpoint URLs whose listeners must run.
type ProxyState struct {
	Listeners []string `json:"listeners"`
}

// GatewayProxy selects the single CONNECT proxy a child uses for every
// gateway request. Worker is the proxy worker's stable identity, recorded for
// display and auditing; the URL alone determines routing.
type GatewayProxy struct {
	URL    string `json:"url"`
	Worker string `json:"worker,omitempty"`
	// Managed records that the invitation selected a Control-managed listener,
	// so the gateway keeps that listener active for this dependency.
	Managed bool `json:"managed,omitempty"`
}

// ValidateGatewayProxy checks display metadata as well as the configured URL.
func ValidateGatewayProxy(proxy *GatewayProxy) error {
	if proxy == nil {
		return nil
	}
	url, err := NormalizeProxyURL(proxy.URL)
	if err != nil {
		return err
	}
	if url != proxy.URL || len(proxy.Worker) > 64 {
		return errors.New("gateway proxy must have a normalized URL and a worker ID of at most 64 bytes")
	}
	return nil
}

// NormalizeProxyURL accepts only an explicit http://host:port CONNECT proxy
// without credentials, path, query, or fragment. IPv6 hosts are bracketed.
func NormalizeProxyURL(value string) (string, error) {
	invalid := errors.New("proxy must be http://HOST:PORT without credentials, path, query, or fragment")
	u, err := url.Parse(strings.TrimSpace(value))
	if err != nil || u.Scheme != "http" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || u.ForceQuery {
		return "", invalid
	}
	host, port, err := net.SplitHostPort(u.Host)
	if err != nil || host == "" || strings.ContainsAny(host, " /\\") {
		return "", invalid
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 || strconv.Itoa(number) != port {
		return "", invalid
	}
	// Strip one root dot first so "0.0.0.0." is checked as an address.
	if ip := net.ParseIP(strings.TrimSuffix(host, ".")); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("proxy address must be a reachable unicast address")
		}
		host = ip.String()
	} else if strings.Contains(host, "%") || strings.Contains(host, ":") {
		// Zoned link-local addresses depend on the child's interface naming.
		return "", errors.New("proxy address must be a hostname, IPv4, or unzoned IPv6 address")
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
		// One root dot is accepted; an empty or still dotted host would not
		// survive its own validation after normalization.
		if host == "" || strings.HasSuffix(host, ".") {
			return "", invalid
		}
	}
	return "http://" + net.JoinHostPort(host, port), nil
}

// ValidateProxyEndpoints checks a worker's advertisement for registration.
func ValidateProxyEndpoints(endpoints []ProxyEndpoint) error {
	if len(endpoints) > MaxProxyEndpoints {
		return errors.New("too many proxy endpoints")
	}
	seen := map[string]bool{}
	for _, endpoint := range endpoints {
		normalized, err := NormalizeProxyURL(endpoint.URL)
		if err != nil {
			return err
		}
		if normalized != endpoint.URL {
			return errors.New("proxy endpoint URL must be normalized")
		}
		if seen[endpoint.URL] {
			return errors.New("duplicate proxy endpoint")
		}
		seen[endpoint.URL] = true
		if len(endpoint.Interface) > MaxProxyInterfaceBytes || strings.ContainsFunc(endpoint.Interface, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return errors.New("invalid proxy interface label")
		}
		if endpoint.Managed {
			if _, err := ManagedListenAddress(endpoint.URL); err != nil {
				return err
			}
		}
	}
	return nil
}

// cgnat covers shared-address VPN overlays such as 100.64.0.0/10.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func privateAddress(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsPrivate() || (a.Is4() && cgnat.Contains(a))
}

// ManagedListenAddress returns the explicit private or VPN socket address a
// managed listener binds. Hostnames, wildcard, loopback, link-local, and
// public addresses are refused so the listener never opens on every interface.
func ManagedListenAddress(proxyURL string) (netip.AddrPort, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return netip.AddrPort{}, errors.New("invalid managed proxy URL")
	}
	addr, err := netip.ParseAddrPort(u.Host)
	if err != nil || addr.Addr().Zone() != "" {
		return netip.AddrPort{}, errors.New("a managed proxy URL must use an explicit IP address and port")
	}
	if !privateAddress(addr.Addr()) {
		return netip.AddrPort{}, errors.New("a managed proxy must listen on a private LAN or VPN address (RFC 1918, 100.64.0.0/10, or fc00::/7)")
	}
	return netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port()), nil
}

// ManagedSources validates a managed listener's source allowlist. Every
// prefix must lie inside a private or VPN range.
func ManagedSources(sources []string) ([]netip.Prefix, error) {
	if len(sources) == 0 {
		return nil, errors.New("a managed proxy requires at least one source CIDR")
	}
	if len(sources) > MaxProxySources {
		return nil, errors.New("too many managed proxy source CIDRs")
	}
	out := make([]netip.Prefix, 0, len(sources))
	for _, source := range sources {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(source))
		if err != nil {
			addr, addrErr := netip.ParseAddr(strings.TrimSpace(source))
			if addrErr != nil {
				return nil, errors.New("invalid managed proxy source CIDR " + strconv.Quote(source))
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		prefix = prefix.Masked()
		if prefix.Addr().Zone() != "" || prefix.Addr().Is4In6() || !privatePrefix(prefix) {
			return nil, errors.New("managed proxy source " + strconv.Quote(source) + " must be inside a private LAN or VPN range")
		}
		out = append(out, prefix)
	}
	return out, nil
}

// privatePrefix requires the whole prefix, not only its first address, to lie
// inside one private or VPN range.
func privatePrefix(p netip.Prefix) bool {
	for _, r := range []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16"), cgnat, netip.MustParsePrefix("fc00::/7")} {
		if r.Addr().BitLen() == p.Addr().BitLen() && p.Bits() >= r.Bits() && r.Contains(p.Addr()) {
			return true
		}
	}
	return false
}

// ValidProxyReadiness bounds a node-reported observation.
func ValidProxyReadiness(readiness []ProxyReadiness) bool {
	if len(readiness) > MaxProxyEndpoints {
		return false
	}
	for _, r := range readiness {
		if (r.State != ProxyReady && r.State != ProxyUnavailable && r.State != ProxyStandby) || len(r.Error) > MaxProxyErrorBytes || len(r.URL) > 256 || r.CheckedAt.IsZero() || r.AgeSeconds < 0 {
			return false
		}
		if r.State != ProxyUnavailable && r.Error != "" {
			return false
		}
	}
	return true
}

// ProxyStatuses projects advertised endpoints with readiness observed at
// observedAt. Configuration without a fresh matching observation is never ready.
func ProxyStatuses(endpoints []ProxyEndpoint, online bool, readiness []ProxyReadiness, observedAt, now time.Time) []ProxyEndpointStatus {
	if len(endpoints) == 0 {
		return nil
	}
	out := make([]ProxyEndpointStatus, 0, len(endpoints))
	for _, endpoint := range endpoints {
		status := ProxyEndpointStatus{URL: endpoint.URL, Interface: endpoint.Interface, Managed: endpoint.Managed, State: ProxyUnchecked}
		if !online {
			status.State = ProxyOffline
			out = append(out, status)
			continue
		}
		for _, r := range readiness {
			// Standby applies only to managed endpoints.
			if r.URL != endpoint.URL || (r.State == ProxyStandby && !endpoint.Managed) {
				continue
			}
			// Date the check on the observer's clock, not the worker's.
			checked := observedAt.Add(-time.Duration(r.AgeSeconds) * time.Second)
			status.State, status.CheckedAt, status.Error = r.State, checked, r.Error
			if now.Sub(checked) > ProxyReadinessTTL {
				status.State = ProxyStale
			}
		}
		out = append(out, status)
	}
	return out
}

// ProxyWorker is a fleet machine advertising gateway proxy endpoints, as
// projected for invitation selection.
type ProxyWorker struct {
	ID        string                `json:"id"`
	Name      string                `json:"name"`
	Online    bool                  `json:"online"`
	Disabled  bool                  `json:"disabled,omitempty"`
	Endpoints []ProxyEndpointStatus `json:"endpoints"`
}

// ReadyEndpoints returns endpoints currently eligible for a new invitation:
// ready endpoints, and managed endpoints on standby that invitation creation
// activates.
func (w ProxyWorker) ReadyEndpoints() []ProxyEndpointStatus {
	var ready []ProxyEndpointStatus
	if !w.Online || w.Disabled {
		return nil
	}
	for _, endpoint := range w.Endpoints {
		if endpoint.Selectable() {
			ready = append(ready, endpoint)
		}
	}
	return ready
}
