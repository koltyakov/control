package transport

import (
	"net"
	"testing"
)

func TestBufferedNetTunesUDPSocketsAndRecordsSmallestBuffer(t *testing.T) {
	stats := &udpBufferStats{}
	network, err := newBufferedNet(stats)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := network.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	recorded := stats.value()
	if recorded <= 0 {
		t.Fatalf("effective receive buffer was not recorded: %d", recorded)
	}
	packet, err := network.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = packet.Close()
	if stats.value() > recorded {
		t.Fatalf("minimum increased from %d to %d", recorded, stats.value())
	}

	stats.record(int(recorded) + 1)
	if stats.value() != recorded {
		t.Fatal("larger socket replaced the smallest effective buffer")
	}
	stats.record(1024)
	if stats.value() != 1024 {
		t.Fatalf("smaller socket was not recorded: %d", stats.value())
	}
	stats.record(0)
	if stats.value() != 1024 {
		t.Fatal("unknown sizes must not reset the minimum")
	}
	var missing *udpBufferStats
	missing.record(1)
	if missing.value() != 0 {
		t.Fatal("nil statistics must stay empty")
	}
}

func TestBufferedNetReusesInterfaceSnapshotBriefly(t *testing.T) {
	network, err := newBufferedNet(nil)
	if err != nil {
		t.Fatal(err)
	}
	first := network.current()
	if network.current() != first {
		t.Fatal("one gathering pass must reuse the same interface snapshot")
	}
	interfaces, err := network.Interfaces()
	if err != nil || len(interfaces) == 0 {
		t.Fatalf("interfaces = %d, %v", len(interfaces), err)
	}
	if _, err := network.InterfaceByName(interfaces[0].Name); err != nil {
		t.Fatal(err)
	}
	network.mu.Lock()
	network.sampled = network.sampled.Add(-interfaceRefresh)
	network.mu.Unlock()
	if network.current() == first {
		t.Fatal("expired interface snapshot was not refreshed")
	}
}
