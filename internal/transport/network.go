package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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
func networkProbe(ctx context.Context, gatewayURL string) func() (string, error) {
	var targets []*net.UDPAddr
	if u, err := url.Parse(gatewayURL); err == nil {
		resolve, cancel := context.WithTimeout(ctx, 2*time.Second)
		addresses, _ := net.DefaultResolver.LookupIPAddr(resolve, u.Hostname())
		cancel()
		for _, address := range addresses[:min(len(addresses), 16)] {
			targets = append(targets, &net.UDPAddr{IP: address.IP, Zone: address.Zone, Port: 443})
		}
	}
	return func() (string, error) {
		interfaces, err := net.Interfaces()
		if err != nil {
			return "", err
		}
		var networks []networkInterface
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addresses, err := iface.Addrs()
			if err != nil {
				return "", err
			}
			networks = append(networks, networkInterface{Interface: iface, addresses: addresses})
		}
		var routes []networkRoute
		for _, target := range targets {
			route := networkRoute{target: target}
			if conn, err := net.DialUDP("udp", nil, target); err == nil {
				route.source = conn.LocalAddr().(*net.UDPAddr).IP
				_ = conn.Close()
			}
			routes = append(routes, route)
		}
		return networkFingerprint(networks, routes)
	}
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
		sample = networkProbe(p.ctx, p.cfg.Gateway)
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
