package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/model"
)

type proxySavedSettings struct {
	Enabled  bool                 `json:"enabled"`
	Settings *model.ProxySettings `json:"settings,omitempty"`
}

// nativeProxyInterfaces discovers addresses on this worker, never on the gateway.
func nativeProxyInterfaces() ([]model.ProxyInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := []model.ProxyInterface{}
	for _, nic := range interfaces {
		if nic.Flags&net.FlagUp == 0 || nic.Flags&net.FlagLoopback != 0 || len(nic.Name) > model.MaxProxyInterfaceBytes || strings.ContainsFunc(nic.Name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			continue
		}
		addresses, err := nic.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || prefix.Addr().Is4In6() {
				continue
			}
			if _, err := model.ManagedListenAddress("http://" + net.JoinHostPort(prefix.Addr().String(), "8080")); err != nil {
				continue
			}
			network := prefix.Masked().String()
			out = append(out, model.ProxyInterface{Name: nic.Name, Address: prefix.Addr().String(), Prefix: network})
			if len(out) == 256 {
				return out, nil
			}
		}
	}
	return out, nil
}

func validateProxyInterface(q model.ProxyConfigure, interfaces []model.ProxyInterface) error {
	if _, err := q.Endpoint(); err != nil {
		return err
	}
	for _, nic := range interfaces {
		if nic.Name == q.Interface && nic.Address == q.Address {
			return nil
		}
	}
	return errors.New("address is not assigned to the selected live interface")
}

func (m *managedProxy) advertisementLocked() []model.ProxyEndpoint {
	out := slices.Clone(m.base)
	if m.enabled && m.settings != nil {
		s := m.settings
		// Legacy profile declarations may have a display label or bare source
		// addresses. New management settings were validated before adoption.
		out = append(out, model.ProxyEndpoint{URL: "http://" + net.JoinHostPort(s.Address, strconv.Itoa(s.Port)), Interface: s.Interface, Managed: true, Sources: s.Sources})
	}
	return out
}

func (m *managedProxy) configuration(interfaces []model.ProxyInterface) model.ProxyConfiguration {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := model.ProxyConfiguration{Supported: true, Enabled: m.enabled, Interfaces: interfaces}
	if m.settings != nil {
		s := *m.settings
		s.Sources = slices.Clone(s.Sources)
		if !m.strictInterface {
			for _, nic := range interfaces {
				if nic.Address == s.Address {
					s.Interface = nic.Name
					break
				}
			}
		}
		out.Settings = &s
	}
	return out
}

// configure binds a replacement before committing settings. A failed bind or
// durable write leaves the previous listener and durable state intact.
func (m *managedProxy) configure(q model.ProxyConfigure, interfaces []model.ProxyInterface) error {
	var endpoint model.ProxyEndpoint
	var next *model.ProxySettings
	if q.Enabled {
		if err := validateProxyInterface(q, interfaces); err != nil {
			return err
		}
		endpoint, _ = q.Endpoint()
		next = &model.ProxySettings{Interface: q.Interface, Address: q.Address, Port: q.Port, Sources: slices.Clone(q.Sources)}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx == nil || m.fenced || m.active != 0 || m.storageUncertain {
		return errors.New("proxy is busy or reserved for maintenance")
	}
	if q.Enabled {
		for _, e := range m.base {
			if e.URL == endpoint.URL {
				return errors.New("address already belongs to another configured endpoint")
			}
		}
		if len(m.base) >= model.MaxProxyEndpoints {
			return errors.New("too many proxy endpoints")
		}
	}
	if next == nil {
		next = m.settings
	}
	var listener net.Listener
	if q.Enabled && m.listeners[endpoint.URL] == nil {
		addr, _ := model.ManagedListenAddress(endpoint.URL)
		var err error
		listener, err = m.listen(m.ctx, addr.String())
		if err != nil {
			return errors.New("could not bind selected proxy address")
		}
	}
	if err := m.write(m.settingsPath, proxySavedSettings{Enabled: q.Enabled, Settings: next}); err != nil {
		if listener != nil {
			_ = listener.Close()
		}
		// A directory sync can fail after rename. Running sockets stay
		// unchanged, but inspection must not falsely reconcile durable state.
		m.storageUncertain = true
		return errors.New("proxy storage outcome is uncertain; recover worker storage before restarting")
	}
	old := m.advertisementLocked()
	m.settings, m.enabled = next, q.Enabled
	m.strictInterface = true
	for _, e := range old {
		if !e.Managed || slices.ContainsFunc(m.base, func(b model.ProxyEndpoint) bool { return b.URL == e.URL }) || (q.Enabled && e.URL == endpoint.URL) {
			continue
		}
		if ln := m.listeners[e.URL]; ln != nil {
			_ = ln.Close()
			delete(m.listeners, e.URL)
		}
		delete(m.endpoints, e.URL)
		delete(m.failures, e.URL)
	}
	if q.Enabled {
		addr, _ := model.ManagedListenAddress(endpoint.URL)
		sources, _ := model.ManagedSources(endpoint.Sources)
		e := managedEndpoint{url: endpoint.URL, addr: addr, sources: sources}
		m.endpoints[e.url] = e
		delete(m.failures, e.url)
		if listener != nil {
			m.listeners[e.url] = listener
			m.wg.Add(1)
			go m.accept(listener, e)
		}
	}
	m.desired = nil
	for u := range m.endpoints {
		m.desired = append(m.desired, u)
	}
	m.gen++
	return nil
}

func (n *Node) configureProxyManagement() {
	n.Peer.SetProxyManagementHandler(n.proxyManagementHandler())
}

func (n *Node) proxyManagementHandler() func([]byte) {
	if n.managedProxy == nil || n.Config.GatewayProxy != nil || !accountMayCall(n.Config.Allow, "proxy.manage") {
		return nil
	}
	seen := map[string]int64{}
	return func(data []byte) {
		var q model.ProxyManagementRequest
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&q) != nil || decoder.Decode(new(any)) != io.EOF || len(q.ID) != 32 {
			return
		}
		ctx, cancel := context.WithTimeout(n.ctx, 8*time.Second)
		defer cancel()
		reply := model.ProxyManagementReply{ID: q.ID}
		for id, expiry := range seen {
			if expiry < time.Now().Unix() {
				delete(seen, id)
			}
		}
		interfaces, err := nativeProxyInterfaces()
		switch {
		case err != nil:
			reply.Error = "interfaces"
		case q.ExpiresAt < time.Now().Unix() || q.ExpiresAt > time.Now().Add(time.Minute).Unix() || seen[q.ID] != 0:
			reply.Error = "expired"
		case len(seen) >= 4096:
			reply.Error = "capacity"
		default:
			seen[q.ID] = q.ExpiresAt
			if q.Configure != nil {
				n.maintenanceMu.Lock()
				_, paused := n.work.State()
				conflict := false
				if q.Configure.Enabled {
					if api, err := netip.ParseAddrPort(n.Config.Listen); err == nil && int(api.Port()) == q.Configure.Port {
						address, _ := netip.ParseAddr(q.Configure.Address)
						conflict = api.Addr().IsUnspecified() || api.Addr() == address
					}
				}
				if conflict {
					reply.Error = "proxy cannot share the local API listener"
				} else if paused {
					reply.Error = "busy"
				} else if done, enterErr := n.work.Enter(ctx); enterErr != nil {
					reply.Error = "denied"
				} else {
					if err := n.managedProxy.configure(*q.Configure, interfaces); err != nil {
						reply.Error = err.Error()
					}
					done()
				}
				n.maintenanceMu.Unlock()
			}
		}
		reply.Configuration = n.managedProxy.configuration(interfaces)
		n.managedProxy.mu.Lock()
		endpoints := n.managedProxy.advertisementLocked()
		if n.managedProxy.storageUncertain {
			reply.Error = "proxy storage outcome is uncertain; recover worker storage before restarting"
		}
		n.managedProxy.mu.Unlock()
		reply.Endpoints = advertisedProxyEndpoints(endpoints)
		n.Peer.SetProxyEndpoints(reply.Endpoints)
		n.proxyChecks.mu.Lock()
		n.proxyChecks.endpoints = endpoints
		n.proxyChecks.mu.Unlock()
		_ = n.Peer.SendControl(ctx, "proxy.result", model.JSON(reply))
		if q.Configure != nil && reply.Error == "" {
			n.proxyChecks.recheck()
		}
	}
}
