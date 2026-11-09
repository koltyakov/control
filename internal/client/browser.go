package client

import (
	"context"
	"errors"
	"net"
	"strconv"
)

// BrowserViewSpec exposes an already started worker dashboard on this host.
// It never launches a browser, retries commands, or changes authentication.
type BrowserViewSpec struct {
	Node   string `json:"node"`
	Port   int    `json:"port,omitempty"`
	Listen string `json:"listen,omitempty"`
}

type BrowserViewInfo struct {
	ForwardInfo
	URL string `json:"url"`
}

// StartBrowserView forwards the dashboard, including its same-origin WebSocket
// proxies. Only loopback listeners are accepted: dashboard access is interactive
// browser authority, not a read-only observation endpoint.
func (c Client) StartBrowserView(ctx context.Context, spec BrowserViewSpec) (*Forward, BrowserViewInfo, error) {
	if spec.Node == "" {
		return nil, BrowserViewInfo{}, errors.New("browser view requires a target machine")
	}
	if spec.Port == 0 {
		spec.Port = 4848
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return nil, BrowserViewInfo{}, errors.New("browser dashboard port must be 1..65535")
	}
	if spec.Listen == "" {
		spec.Listen = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(spec.Listen)
	if err != nil {
		return nil, BrowserViewInfo{}, errors.New("browser view listen must be a loopback IP and port")
	}
	ip := net.ParseIP(host)
	p, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || p < 0 || p > 65535 {
		return nil, BrowserViewInfo{}, errors.New("browser view listen must be a loopback IP and port")
	}
	// Pin the destination before publishing a listener. A later name reuse must
	// not redirect an existing interactive browser view to a different node.
	peer, err := c.backend(ctx)
	if err != nil {
		return nil, BrowserViewInfo{}, err
	}
	if peer != nil {
		node, err := peer.Lookup(ctx, spec.Node)
		if err != nil {
			return nil, BrowserViewInfo{}, err
		}
		spec.Node = node.ID
	}
	f, err := c.StartForward(ctx, ForwardSpec{
		Node: spec.Node, Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.Port)), Listen: spec.Listen,
	})
	if err != nil {
		return nil, BrowserViewInfo{}, err
	}
	info := f.Info()
	return f, BrowserViewInfo{ForwardInfo: info, URL: "http://" + info.Listen}, nil
}
