// Package transporttest supplies isolated transport construction for local tests.
// It is imported only by test files, not deployed Control components.
package transporttest

import (
	"net"

	"github.com/koltyakov/control/internal/transport"
	"github.com/pion/webrtc/v4"
)

// Loopback keeps local peer tests independent of host LAN, VPN and bridge
// interfaces while preserving real ICE, DTLS, pinned TLS and multiplexed RPCs.
func Loopback() transport.Options {
	return transport.Options{ConfigureWebRTC: func(settings *webrtc.SettingEngine) {
		settings.SetIncludeLoopbackCandidate(true)
		settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	}}
}
