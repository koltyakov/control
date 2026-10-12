package transport

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	networkPollInterval    = 2 * time.Second
	carrierDisconnectGrace = 3 * time.Second
)

// Interface/address changes cover VPN adapters, Wi-Fi switches, and DHCP.
// Route-selected source addresses also detect default-route switches that leave
// the adapters unchanged. UDP connect asks the OS for a route; no packets are sent.
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
		var parts []string
		for _, iface := range interfaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addresses, err := iface.Addrs()
			if err != nil {
				return "", err
			}
			for _, address := range addresses {
				ip, _, err := net.ParseCIDR(address.String())
				if err == nil && ip.IsGlobalUnicast() {
					parts = append(parts, fmt.Sprintf("%d/%s/%s", iface.Index, iface.Name, address))
				}
			}
		}
		for _, target := range targets {
			local := "unreachable"
			if conn, err := net.DialUDP("udp", nil, target); err == nil {
				local = conn.LocalAddr().(*net.UDPAddr).IP.String()
				_ = conn.Close()
			}
			parts = append(parts, target.IP.String()+"="+local)
		}
		sort.Strings(parts)
		return strings.Join(parts, "\n"), nil
	}
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
