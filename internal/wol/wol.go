// Package wol sends bounded Wake-on-LAN packets using verified LAN relationships.
package wol

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/koltyakov/control/internal/model"
)

const MaxInterfaces = 32

func parse(i model.WakeInterface) (netip.Prefix, net.HardwareAddr, error) {
	p, err := netip.ParsePrefix(i.Address)
	if err != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() < 8 || p.Bits() > 30 {
		return netip.Prefix{}, nil, errors.New("wake interface requires a private IPv4 address with an /8../30 prefix")
	}
	mac, err := net.ParseMAC(i.MAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || string(mac) == string(make([]byte, 6)) {
		return netip.Prefix{}, nil, errors.New("wake interface requires a six-byte unicast MAC")
	}
	if p.Addr() == p.Masked().Addr() || p.Addr() == broadcast(p) || !p.Masked().Addr().IsPrivate() || !broadcast(p).IsPrivate() {
		return netip.Prefix{}, nil, errors.New("invalid wake interface subnet or host address")
	}
	return p, mac, nil
}

func Validate(interfaces []model.WakeInterface) error {
	if len(interfaces) > MaxInterfaces {
		return errors.New("at most 32 wake interfaces are allowed")
	}
	for _, i := range interfaces {
		if _, _, err := parse(i); err != nil {
			return err
		}
	}
	return nil
}

// Interfaces ignores loopback, point-to-point, non-broadcast and non-Ethernet adapters.
// Availability here does not imply that firmware or the NIC supports waking.
func Interfaces() ([]model.WakeInterface, error) {
	adapters, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []model.WakeInterface
	for _, adapter := range adapters {
		if adapter.Flags&net.FlagUp == 0 || adapter.Flags&net.FlagBroadcast == 0 || adapter.Flags&(net.FlagLoopback|net.FlagPointToPoint) != 0 {
			continue
		}
		addresses, err := adapter.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			i := model.WakeInterface{Address: address.String(), MAC: adapter.HardwareAddr.String()}
			if _, _, err := parse(i); err == nil && len(result) < MaxInterfaces {
				result = append(result, i)
			}
		}
	}
	return result, nil
}

func broadcast(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	v := binary.BigEndian.Uint32(a[:]) | uint32((uint64(1)<<(32-p.Bits()))-1)
	binary.BigEndian.PutUint32(a[:], v)
	return netip.AddrFrom4(a)
}

type packet struct {
	local netip.Addr
	to    netip.Addr
	data  [102]byte
}

func plan(sourceID string, local []model.WakeInterface, target model.Node) ([]packet, error) {
	if err := Validate(local); err != nil {
		return nil, err
	}
	if err := Validate(target.WakeInterfaces); err != nil {
		return nil, err
	}
	var packets []packet
	seen := map[string]bool{}
	for _, remote := range target.WakeInterfaces {
		p, mac, _ := parse(remote)
		for _, source := range local {
			s, _, _ := parse(source)
			key := p.Masked().String() + "/" + mac.String()
			if s.Masked() != p.Masked() || s.Addr() == p.Addr() || seen[key] {
				continue
			}
			// A matching private prefix is only a candidate, never LAN evidence.
			if !relationship(target, source, remote, sourceID, time.Now()) {
				continue
			}
			seen[key] = true
			q := packet{local: s.Addr(), to: broadcast(s)}
			for j := range 6 {
				q.data[j] = 0xff
			}
			for j := range 16 {
				copy(q.data[6+j*6:], mac)
			}
			packets = append(packets, q)
		}
	}
	if len(packets) == 0 {
		return nil, errors.New("no verified LAN relationship for these live interfaces")
	}
	return packets, nil
}

func relationship(target model.Node, source, remote model.WakeInterface, id string, now time.Time) bool {
	for _, peer := range target.LANPeers {
		if peer.ID == id && peer.Local == remote && peer.Remote == source && !peer.VerifiedAt.After(now) && now.Sub(peer.VerifiedAt) < model.LANPeerTTL {
			return true
		}
	}
	return false
}

// SameSubnet selects discovery candidates only. It does not prove a shared LAN.
func SameSubnet(a, b model.WakeInterface) bool {
	x, _, err := parse(a)
	y, _, other := parse(b)
	return err == nil && other == nil && x.Masked() == y.Masked() && x.Addr() != y.Addr()
}

// Broadcast sends a bounded discovery proof from an eligible local interface.
func Broadcast(ctx context.Context, source model.WakeInterface, port int, data []byte) error {
	prefix, _, err := parse(source)
	if err != nil || port < 1024 || port > 65535 || len(data) > 2048 {
		return errors.New("invalid LAN discovery datagram")
	}
	return writeUDP(ctx, prefix.Addr(), broadcast(prefix), fmt.Sprint(port), data)
}

// CanSend is a selection hint. Send revalidates against live local interfaces.
func CanSend(source, target model.Node) bool {
	_, err := plan(source.ID, source.WakeInterfaces, target)
	return err == nil
}

// Send transmits one UDP port 9 packet per distinct matching MAC/subnet.
// A successful UDP write does not acknowledge receipt or a machine waking.
func Send(ctx context.Context, source string, target model.Node) (int, error) {
	local, err := Interfaces()
	if err != nil {
		return 0, err
	}
	packets, err := plan(source, local, target)
	if err != nil {
		return 0, err
	}
	return send(ctx, packets, writePacket)
}

func send(ctx context.Context, packets []packet, write func(context.Context, packet) error) (int, error) {
	for i, p := range packets {
		if err := ctx.Err(); err != nil {
			return i, err
		}
		if err := write(ctx, p); err != nil {
			return i, fmt.Errorf("wake packet failed after %d sends; do not retry automatically: %w", i, err)
		}
	}
	return len(packets), nil
}

func writePacket(ctx context.Context, p packet) error {
	return writeDatagram(ctx, p, "9")
}

func writeDatagram(ctx context.Context, p packet, port string) error {
	return writeUDP(ctx, p.local, p.to, port, p.data[:])
}

func writeUDP(ctx context.Context, local, target netip.Addr, port string, data []byte) error {
	d := net.Dialer{LocalAddr: &net.UDPAddr{IP: net.IP(local.AsSlice())}}
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(target.String(), port))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(2 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	if err = conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	_, err = conn.Write(data)
	return err
}
