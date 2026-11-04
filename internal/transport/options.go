package transport

import "github.com/pion/webrtc/v4"

// Options supplies per-peer construction settings, not serialized node or wire
// configuration. The zero value retains the production networking defaults.
type Options struct {
	// ConfigureWebRTC customizes each peer's legacy and trickle setting engines
	// after the transport defaults are applied and before its APIs are created.
	ConfigureWebRTC func(*webrtc.SettingEngine) `json:"-"`
}
