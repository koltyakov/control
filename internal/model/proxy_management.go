package model

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
)

// ProxyInterface is an eligible address observed on a live worker interface.
type ProxyInterface struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Prefix  string `json:"prefix"`
}

type ProxySettings struct {
	Interface string   `json:"interface"`
	Address   string   `json:"address"`
	Port      int      `json:"port"`
	Sources   []string `json:"sources"`
}

type ProxyConfiguration struct {
	Supported  bool             `json:"supported"`
	Enabled    bool             `json:"enabled"`
	Interfaces []ProxyInterface `json:"interfaces"`
	Settings   *ProxySettings   `json:"settings,omitempty"`
	Dependents ProxyDependents  `json:"dependents"`
}

type ProxyConfigure struct {
	Enabled   bool     `json:"enabled"`
	Interface string   `json:"interface,omitempty"`
	Address   string   `json:"address,omitempty"`
	Port      int      `json:"port,omitempty"`
	Sources   []string `json:"sources,omitempty"`
}

func (q ProxyConfigure) Endpoint() (ProxyEndpoint, error) {
	if q.Interface == "" || q.Port < 1 || q.Port > 65535 {
		return ProxyEndpoint{}, errors.New("select an interface, address, and port 1..65535")
	}
	e := ProxyEndpoint{URL: "http://" + net.JoinHostPort(q.Address, strconv.Itoa(q.Port)), Interface: q.Interface, Managed: true, Sources: q.Sources}
	if err := ValidateProxyEndpoints([]ProxyEndpoint{e}); err != nil {
		return ProxyEndpoint{}, err
	}
	// Management requires explicit CIDRs, even though legacy configuration
	// also accepts bare host addresses.
	for _, source := range q.Sources {
		if _, err := netip.ParsePrefix(source); err != nil {
			return ProxyEndpoint{}, errors.New("sources must be explicit private CIDRs")
		}
	}
	_, err := ManagedSources(q.Sources)
	return e, err
}

// ProxyManagementRequest is a fixed owner-management operation with no RPC,
// executable, credential, or arbitrary filesystem access.
type ProxyManagementRequest struct {
	ID        string          `json:"id"`
	ExpiresAt int64           `json:"expiresAt"`
	Configure *ProxyConfigure `json:"configure,omitempty"`
}

type ProxyManagementReply struct {
	ID            string             `json:"id"`
	Error         string             `json:"error,omitempty"`
	Configuration ProxyConfiguration `json:"configuration"`
	Endpoints     []ProxyEndpoint    `json:"endpoints"`
}
