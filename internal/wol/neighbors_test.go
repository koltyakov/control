package wol

import (
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestNeighborParsers(t *testing.T) {
	unix := parseUnixNeighbors([]byte("? (192.168.0.117) at dc:45:46:f4:71:a8 on en0 ifscope [ethernet]\n? (192.168.0.115) at c8:ff:bf:1:22:80 on en0 [ethernet]\n? (192.168.0.118) at (incomplete) on en0\n"))
	if len(unix) != 2 || unix[1].MAC != "c8:ff:bf:01:22:80" || unix[0].Device != "en0" {
		t.Fatal(unix)
	}
	windows := parseWindowsNeighbors([]byte("Interface: 192.168.0.217 --- 0x13\n  Internet Address Physical Address Type\n 192.168.0.117 dc-45-46-f4-71-a8 dynamic\nInterface: 192.168.64.1 --- 0x21\n 192.168.64.2 aa-bb-cc-dd-ee-ff dynamic\n"))
	if len(windows) != 2 || windows[0].Device != "192.168.0.217" || windows[0].MAC != "dc:45:46:f4:71:a8" || windows[1].Device != "192.168.64.1" {
		t.Fatal(windows)
	}
	linux, err := parseLinuxNeighbors([]byte(`[{"dst":"192.168.0.117","dev":"eth0","lladdr":"dc:45:46:f4:71:a8","state":["REACHABLE"]},{"dst":"192.168.0.118","dev":"eth0","state":["INCOMPLETE"]},{"dst":"192.168.0.119","dev":"eth0","lladdr":"dc:45:46:f4:71:a8","state":["FAILED"]}]`))
	if err != nil || len(linux) != 1 || linux[0].Device != "eth0" {
		t.Fatal(linux, err)
	}
	if _, err := parseLinuxNeighbors([]byte(`not json`)); err == nil {
		t.Fatal("accepted malformed neighbor table")
	}
	for _, bad := range []string{"", "incomplete", "00:11:22", "gg:00:00:00:00:00", "000:11:22:33:44:55:66"} {
		if normalizeMAC(bad) != "" {
			t.Fatal("accepted bad MAC", bad)
		}
	}
}

func TestStandaloneLocalWakeUsesExactNeighborEvidence(t *testing.T) {
	now := time.Now()
	local := model.WakeInterface{Address: "192.168.0.217/24", MAC: "fa:98:35:dc:d3:21"}
	remote := model.WakeInterface{Address: "192.168.0.117/24", MAC: "dc:45:46:f4:71:a8"}
	target := model.Node{ID: "sleeping", WakeInterfaces: []model.WakeInterface{remote}}
	devices := map[model.WakeInterface]string{local: "en0"}
	table := []neighbor{{IP: "192.168.0.117", MAC: remote.MAC, Device: "en0"}}
	derived, ok := localTarget([]model.WakeInterface{local}, devices, table, target, nil, now)
	if !ok || len(target.LANPeers) != 0 || !CanSend(model.Node{ID: "local", WakeInterfaces: []model.WakeInterface{local}}, derived) {
		t.Fatal("standalone host was excluded or target was mutated", derived)
	}
	for _, wrong := range []neighbor{
		{IP: "192.168.0.117", MAC: "aa:bb:cc:dd:ee:ff", Device: "en0"},
		{IP: "192.168.0.118", MAC: remote.MAC, Device: "en0"},
		{IP: "192.168.0.117", MAC: remote.MAC, Device: "bridge100"},
	} {
		if _, ok := localTarget([]model.WakeInterface{local}, devices, []neighbor{wrong}, target, nil, now); ok {
			t.Fatal("wrong neighbor accepted", wrong)
		}
	}
	if _, ok := localTarget([]model.WakeInterface{local}, devices, nil, target, nil, now); ok {
		t.Fatal("subnet alone was treated as LAN evidence")
	}
	// Windows identifies the local interface by its IPv4 address, not its name.
	table[0].Device = "192.168.0.217"
	if _, ok := localTarget([]model.WakeInterface{local}, devices, table, target, nil, now); !ok {
		t.Fatal("Windows interface binding rejected")
	}
}

func TestStandaloneLocalWakeViaVerifiedWitness(t *testing.T) {
	now := time.Now()
	local := model.WakeInterface{Address: "192.168.0.217/24", MAC: "02:00:00:00:00:01"}
	remote := model.WakeInterface{Address: "192.168.0.117/24", MAC: "02:00:00:00:00:02"}
	witnessNIC := model.WakeInterface{Address: "192.168.0.115/24", MAC: "02:00:00:00:00:03"}
	target := model.Node{ID: "sleeping", WakeInterfaces: []model.WakeInterface{remote}, LANPeers: []model.LANPeer{{ID: "witness", Local: remote, Remote: witnessNIC, VerifiedAt: now.Add(-time.Minute)}}}
	witness := model.Node{ID: "witness", Online: true, WakeInterfaces: []model.WakeInterface{witnessNIC}}
	devices := map[model.WakeInterface]string{local: "en0"}
	table := []neighbor{{IP: "192.168.0.115", MAC: witnessNIC.MAC, Device: "en0"}}
	if _, ok := localTarget([]model.WakeInterface{local}, devices, table, target, []model.Node{witness}, now); !ok {
		t.Fatal("same-LAN witness was not recognized")
	}
	for _, change := range []func(*model.Node){
		func(n *model.Node) { n.ID = "other" },
		func(n *model.Node) { n.Online = false },
		func(n *model.Node) { n.WakeInterfaces = nil },
	} {
		n := witness
		change(&n)
		if _, ok := localTarget([]model.WakeInterface{local}, devices, table, target, []model.Node{n}, now); ok {
			t.Fatal("invalid witness accepted", n)
		}
	}
	target.LANPeers[0].VerifiedAt = now.Add(-model.LANPeerTTL)
	if _, ok := localTarget([]model.WakeInterface{local}, devices, table, target, []model.Node{witness}, now); ok {
		t.Fatal("expired relationship accepted")
	}
}

func TestNeighborOutputBound(t *testing.T) {
	var output neighborOutput
	if _, err := output.Write([]byte(strings.Repeat("x", 1<<20))); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte("x")); err == nil {
		t.Fatal("unbounded neighbor output")
	}
}
