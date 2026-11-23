package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	networkPollInterval    = 2 * time.Second
	carrierDisconnectGrace = 3 * time.Second
)

type networkInterface struct {
	net.Interface
	addresses []net.Addr
}

type networkRoute struct {
	target *net.UDPAddr
	source net.IP // nil means the target is unreachable.
}

// Track gateway egress networks, not the host's entire address inventory. IPv6
// privacy addresses can alternate on one interface/subnet without changing any
// established connection. UDP connect asks the OS for a route; no packets are sent.
// A proxied node watches its route to the proxy: the gateway may not resolve.
func networkProbe(ctx context.Context, host, port string) func() (string, error) {
	sampler := &networkSampler{interfaces: sampleInterfaces, route: routeSource, now: time.Now}
	if host != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number <= 0 {
			number = 443
		}
		sampler.resolve = func() []*net.UDPAddr {
			resolve, cancel := context.WithTimeout(ctx, 2*time.Second)
			addresses, _ := net.DefaultResolver.LookupIPAddr(resolve, host)
			cancel()
			var targets []*net.UDPAddr
			for _, address := range addresses[:min(len(addresses), 16)] {
				targets = append(targets, &net.UDPAddr{IP: address.IP, Zone: address.Zone, Port: number})
			}
			return targets
		}
	}
	return sampler.sample
}

// networkResolveRetry spaces gateway lookups while none has succeeded, such as
// after booting without DNS. Resolved targets are kept: re-resolving a healthy
// route could change the fingerprint without any network change.
const networkResolveRetry = 30 * time.Second

// networkInterfaceRefresh bounds reuse of an interface inventory while every
// route keeps its source address. Enumeration is the expensive part of a
// sample: on Windows each interface's addresses cost another adapter query,
// together milliseconds of CPU every poll on an idle node.
const networkInterfaceRefresh = 30 * time.Second

// networkSampler resolves routes on every poll and re-enumerates interfaces
// only when a source address changes, the cached inventory cannot place a
// source, or the inventory is older than networkInterfaceRefresh. A changed
// source still alters the fingerprint immediately. Address-only changes on an
// unchanged source, such as a subnet edit, are noticed within the refresh.
type networkSampler struct {
	targets    []*net.UDPAddr
	resolve    func() []*net.UDPAddr
	resolveAt  time.Time
	interfaces func() ([]networkInterface, error)
	route      func(*net.UDPAddr) net.IP
	now        func() time.Time

	cached     []networkInterface
	sources    string
	enumerated time.Time
}

func (s *networkSampler) sample() (string, error) {
	if len(s.targets) == 0 && s.resolve != nil && !s.now().Before(s.resolveAt) {
		s.targets, s.resolveAt = s.resolve(), s.now().Add(networkResolveRetry)
	}
	if len(s.targets) == 0 {
		return "", nil
	}
	routes := make([]networkRoute, 0, len(s.targets))
	sources := make([]string, 0, len(s.targets))
	for _, target := range s.targets {
		source := s.route(target)
		routes = append(routes, networkRoute{target: target, source: source})
		sources = append(sources, source.String())
	}
	key := strings.Join(sources, ",")
	now := s.now()
	fresh := false
	if s.cached == nil || key != s.sources || now.Sub(s.enumerated) >= networkInterfaceRefresh {
		if err := s.enumerate(now); err != nil {
			return "", err
		}
		fresh = true
	}
	s.sources = key
	fingerprint, err := networkFingerprint(s.cached, routes)
	if err != nil && !fresh {
		// The cached inventory may predate a new address on the same source.
		if err := s.enumerate(now); err != nil {
			return "", err
		}
		fingerprint, err = networkFingerprint(s.cached, routes)
	}
	return fingerprint, err
}

func (s *networkSampler) enumerate(now time.Time) error {
	interfaces, err := s.interfaces()
	if err != nil {
		return err
	}
	s.cached, s.enumerated = interfaces, now
	return nil
}

func sampleInterfaces() ([]networkInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	networks := []networkInterface{}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		networks = append(networks, networkInterface{Interface: iface, addresses: addresses})
	}
	return networks, nil
}

// routeSource returns nil when the target is unreachable.
func routeSource(target *net.UDPAddr) net.IP {
	conn, err := net.DialUDP("udp", nil, target)
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()
	return conn.LocalAddr().(*net.UDPAddr).IP
}

func networkFingerprint(interfaces []networkInterface, routes []networkRoute) (string, error) {
	var parts []string
	for _, route := range routes {
		local := "unreachable"
		if route.source != nil {
			var matches []string
			for _, iface := range interfaces {
				if iface.Flags&net.FlagUp == 0 || (route.target.Zone != "" && route.target.Zone != iface.Name && route.target.Zone != strconv.Itoa(iface.Index)) {
					continue
				}
				for _, address := range iface.addresses {
					ip, subnet, err := net.ParseCIDR(address.String())
					if err != nil || !ip.Equal(route.source) {
						continue
					}
					// IPv4 DHCP changes still invalidate the route. For IPv6,
					// compare the assigned subnet rather than the interface ID.
					identity := ip.String() + "@" + subnet.String()
					if ip.To4() == nil {
						identity = subnet.String()
					}
					matches = append(matches, fmt.Sprintf("%d/%s/%s", iface.Index, iface.Name, identity))
				}
			}
			if len(matches) == 0 {
				// Interface enumeration and route lookup are not atomic. Retry
				// the sample rather than interpreting a partial view as a switch.
				return "", errors.New("route source is absent from sampled interfaces")
			}
			slices.Sort(matches)
			local = strings.Join(slices.Compact(matches), ",")
		}
		parts = append(parts, route.target.String()+"="+local)
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), "\n"), nil
}

func (p *Peer) watchNetwork() {
	sample := p.networkSnapshot
	if sample == nil {
		host, port := p.cfg.Route.MonitorHost(p.cfg.Gateway)
		sample = networkProbe(p.ctx, host, port)
	}
	ticker := time.NewTicker(networkPollInterval)
	defer ticker.Stop()
	p.monitorNetwork(sample, ticker.C)
}

func (p *Peer) monitorNetwork(sample func() (string, error), ticks <-chan time.Time) {
	previous, err := sample()
	initialized := err == nil
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticks:
			current, err := sample()
			if err != nil {
				continue // A failed sample is not a network switch.
			}
			if initialized && current != previous {
				p.log.Info("network changed; reconnecting gateway and retiring peer sessions")
				p.resetNetwork()
			}
			previous, initialized = current, true
		}
	}
}

// republishRegistration reconnects only the gateway socket so the next signed
// registration carries fresh metadata. Unlike resetNetwork, it keeps direct
// sessions, carriers, and directory caches. Relay links share the socket and
// close as in any gateway reconnect; their streams are not replayed.
func (p *Peer) republishRegistration() {
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	ws := p.ws
	select {
	case p.reconnectWake <- struct{}{}:
	default:
	}
	p.mu.Unlock()
	if ws != nil {
		_ = ws.CloseNow() // The run loop clears state and re-registers.
	}
}

func (p *Peer) resetNetwork() {
	p.mu.Lock()
	if p.closed || p.ctx.Err() != nil {
		p.mu.Unlock()
		return
	}
	p.networkGeneration++
	select {
	case p.reconnectWake <- struct{}{}:
	default:
	}
	p.directoryGeneration++
	p.resolved = map[string]string{}
	p.lookups = map[string]lookupEntry{}
	p.directRetry = map[string]time.Time{}
	ws := p.ws
	p.ws = nil
	p.lanViewSession = ""
	p.gatewayWS.Store(nil)
	if p.connectCancel != nil {
		p.connectCancel()
	}
	links := make([]*link, 0, len(p.links))
	for _, l := range p.links {
		links = append(links, l)
		if l.carrier != nil {
			l.carrier.closed = true
		}
	}
	sessions := make([]*peerSession, 0, len(p.allSessions))
	for _, s := range p.allSessions {
		sessions = append(sessions, s)
	}
	p.carriers = map[string]*rtcCarrier{}
	p.sessions = map[sessionKey]*peerSession{}
	p.allSessions = map[string]*peerSession{}
	for _, setup := range p.pending {
		setup.cancel()
	}
	p.pending = map[sessionKey]*sessionDial{}
	for _, setup := range p.incomingSetups {
		setup.cancel()
	}
	p.mu.Unlock()
	// Pooled gateway connections may still be bound to the previous network.
	p.cfg.Route.CloseIdleConnections()
	if ws != nil {
		_ = ws.CloseNow() // Wakes the existing, bounded gateway reconnect loop.
	}
	for _, l := range links {
		_ = l.conn.Close()
	}
	for _, s := range sessions {
		_ = s.mux.Close()
	}
}
