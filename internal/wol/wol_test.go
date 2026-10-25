package wol

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func adapter(address, mac string) model.WakeInterface {
	return model.WakeInterface{Address: address, MAC: mac}
}

func TestMagicPacketAndSubnet(t *testing.T) {
	local := []model.WakeInterface{adapter("192.168.4.2/24", "02:00:00:00:00:01"), adapter("192.168.4.3/24", "02:00:00:00:00:02")}
	remote := adapter("192.168.4.10/24", "02:ab:cd:ef:01:23")
	target := model.Node{WakeInterfaces: []model.WakeInterface{remote, remote}, LANPeers: []model.LANPeer{{ID: "sender", Local: remote, Remote: local[0], VerifiedAt: time.Now().Add(-time.Second)}}}
	packets, err := plan("sender", local, target)
	if err != nil || len(packets) != 1 {
		t.Fatal(packets, err)
	}
	p := packets[0]
	if p.to.String() != "192.168.4.255" || p.local.String() != "192.168.4.2" {
		t.Fatal("incorrect route", p)
	}
	mac, _ := net.ParseMAC(remote.MAC)
	want := append(bytes.Repeat([]byte{0xff}, 6), bytes.Repeat(mac, 16)...)
	if !bytes.Equal(want, p.data[:]) {
		t.Fatalf("incorrect magic packet: %x", p.data)
	}
	for _, change := range []func(*model.Node){
		func(n *model.Node) { n.LANPeers = nil },
		func(n *model.Node) {
			n.LANPeers = []model.LANPeer{{ID: "other", Local: remote, Remote: local[0], VerifiedAt: time.Now()}}
		},
		func(n *model.Node) {
			n.LANPeers = []model.LANPeer{{ID: "sender", Local: remote, Remote: local[0], VerifiedAt: time.Now().Add(-model.LANPeerTTL)}}
		},
		func(n *model.Node) { n.WakeInterfaces = nil },
		func(n *model.Node) { n.WakeInterfaces = []model.WakeInterface{adapter("192.168.5.10/24", remote.MAC)} },
		func(n *model.Node) { n.WakeInterfaces = []model.WakeInterface{adapter("192.168.4.10/16", remote.MAC)} },
		func(n *model.Node) { n.WakeInterfaces = []model.WakeInterface{adapter("192.168.4.2/24", remote.MAC)} },
	} {
		n := target
		change(&n)
		if _, err := plan("sender", local[:1], n); err == nil {
			t.Fatal("accepted missing or mismatched LAN/subnet", n)
		}
	}
}

func TestInterfaceValidation(t *testing.T) {
	for _, address := range []string{"", "192.168.1.1", "2001:db8::1/64", "127.0.0.1/8", "8.8.8.8/24", "169.254.1.2/16", "192.168.1.1/0", "10.0.0.1/7", "10.0.0.1/31", "10.0.0.1/32", "192.168.1.0/24", "192.168.1.255/24"} {
		if Validate([]model.WakeInterface{adapter(address, "02:ab:cd:ef:01:23")}) == nil {
			t.Fatal("accepted address", address)
		}
	}
	for _, mac := range []string{"", "bad", "00:00:00:00:00:00", "ff:ff:ff:ff:ff:ff", "01:00:00:00:00:01", "02:00:00:00:00:00:00:01"} {
		if Validate([]model.WakeInterface{adapter("10.1.2.3/24", mac)}) == nil {
			t.Fatal("accepted MAC", mac)
		}
	}
	if Validate(make([]model.WakeInterface, MaxInterfaces+1)) == nil {
		t.Fatal("accepted too many interfaces")
	}
	for _, address := range []string{"10.1.2.3/8", "172.16.1.2/16", "192.168.1.2/30"} {
		if err := Validate([]model.WakeInterface{adapter(address, "02:ab:cd:ef:01:23")}); err != nil {
			t.Fatal(address, err)
		}
	}
}

func TestSendStopsWithoutReplay(t *testing.T) {
	calls := 0
	failure := errors.New("write failed")
	write := func(context.Context, packet) error {
		calls++
		if calls == 2 {
			return failure
		}
		return nil
	}
	n, err := send(context.Background(), make([]packet, 3), write)
	if n != 1 || !errors.Is(err, failure) || calls != 2 {
		t.Fatal(n, err, calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if n, err := send(ctx, make([]packet, 3), write); n != 0 || !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatal(n, err, calls)
	}
}

func TestDatagramOnLoopback(t *testing.T) {
	listener, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	p := packet{local: netip.MustParseAddr("127.0.0.1"), to: netip.MustParseAddr("127.0.0.1")}
	copy(p.data[:], "loopback-only packet fixture")
	if err := writeDatagram(ctx, p, strconv.Itoa(listener.LocalAddr().(*net.UDPAddr).Port)); err != nil {
		t.Fatal(err)
	}
	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 103)
	n, source, err := listener.ReadFromUDP(buf)
	if err != nil || !source.IP.IsLoopback() || !bytes.Equal(buf[:n], p.data[:]) {
		t.Fatal("incorrect datagram", n, source, err)
	}
	cancel()
	if err := writeDatagram(ctx, p, strconv.Itoa(listener.LocalAddr().(*net.UDPAddr).Port)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled datagram was sent", err)
	}
}
