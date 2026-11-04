package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestWebRTCOptionsArePerPeerAndPreserveDefaults(t *testing.T) {
	t.Parallel()
	calls := 0
	p := New(Config{}, nil, Options{ConfigureWebRTC: func(settings *webrtc.SettingEngine) {
		calls++
		settings.SetIncludeLoopbackCandidate(true)
		settings.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
		settings.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	}})
	plain := New(Config{}, nil)
	if calls != 2 || p.cfg.DirectTimeout != 5*time.Second || plain.cfg.DirectTimeout != p.cfg.DirectTimeout || plain.api == p.api || plain.trickleAPI == p.trickleAPI {
		t.Fatal("construction options leaked between peers or changed the default timeout")
	}
	for _, api := range []*webrtc.API{p.api, p.trickleAPI} {
		pc, err := api.NewPeerConnection(webrtc.Configuration{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pc.Close() })
		if _, err = pc.CreateDataChannel("fixture", nil); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		t.Cleanup(cancel)
		candidates := make(chan *webrtc.ICECandidate, 8)
		pc.OnICECandidate(func(candidate *webrtc.ICECandidate) {
			select {
			case candidates <- candidate:
			case <-ctx.Done():
			}
		})
		offer, err := pc.CreateOffer(nil)
		if err != nil {
			t.Fatal(err)
		}
		if err = pc.SetLocalDescription(offer); err != nil {
			t.Fatal(err)
		}
		count := 0
	gather:
		for {
			select {
			case candidate := <-candidates:
				if candidate == nil {
					break gather
				}
				count++
				ip := net.ParseIP(candidate.Address)
				if !ip.IsLoopback() || ip.To4() == nil || candidate.Protocol != webrtc.ICEProtocolUDP || candidate.Typ != webrtc.ICECandidateTypeHost {
					t.Fatal("construction option admitted a non-loopback UDP4 host candidate", candidate)
				}
			case <-ctx.Done():
				t.Fatal("loopback candidate gathering did not complete")
			}
		}
		if count == 0 {
			t.Fatal("loopback construction produced no candidates")
		}
		cancel()
	}
}
