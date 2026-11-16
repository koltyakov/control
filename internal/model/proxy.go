package model

import (
	"errors"
	"net"
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
	// ProxyCheckInterval is the worker's own proxy-to-gateway check cadence,
	// independent of dashboard polling.
	ProxyCheckInterval = time.Minute
	// ProxyReadinessTTL turns an older observation into "stale", which is
	// never eligible for new invitations.
	ProxyReadinessTTL = 3 * ProxyCheckInterval
)

// Proxy readiness states. Only ProxyReady is eligible for invitation selection.
const (
	ProxyReady       = "ready"
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
	State     string    `json:"state"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Error     string    `json:"error,omitempty"`
}

// GatewayProxy selects the single CONNECT proxy a child uses for every
// gateway request. Worker is the proxy worker's stable identity, recorded for
// display and auditing; the URL alone determines routing.
type GatewayProxy struct {
	URL    string `json:"url"`
	Worker string `json:"worker,omitempty"`
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
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsMulticast() {
			return "", errors.New("proxy address must be a reachable unicast address")
		}
		host = ip.String()
	} else if strings.Contains(host, "%") || strings.Contains(host, ":") {
		// Zoned link-local addresses depend on the child's interface naming.
		return "", errors.New("proxy address must be a hostname, IPv4, or unzoned IPv6 address")
	} else {
		host = strings.ToLower(strings.TrimSuffix(host, "."))
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
	}
	return nil
}

// ValidProxyReadiness bounds a node-reported observation.
func ValidProxyReadiness(readiness []ProxyReadiness) bool {
	if len(readiness) > MaxProxyEndpoints {
		return false
	}
	for _, r := range readiness {
		if (r.State != ProxyReady && r.State != ProxyUnavailable) || len(r.Error) > MaxProxyErrorBytes || len(r.URL) > 256 || r.CheckedAt.IsZero() || r.AgeSeconds < 0 {
			return false
		}
		if r.State == ProxyReady && r.Error != "" {
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
		status := ProxyEndpointStatus{URL: endpoint.URL, Interface: endpoint.Interface, State: ProxyUnchecked}
		if !online {
			status.State = ProxyOffline
			out = append(out, status)
			continue
		}
		for _, r := range readiness {
			if r.URL != endpoint.URL {
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

// ReadyEndpoints returns endpoints currently eligible for a new invitation.
func (w ProxyWorker) ReadyEndpoints() []ProxyEndpointStatus {
	var ready []ProxyEndpointStatus
	if !w.Online || w.Disabled {
		return nil
	}
	for _, endpoint := range w.Endpoints {
		if endpoint.State == ProxyReady {
			ready = append(ready, endpoint)
		}
	}
	return ready
}
