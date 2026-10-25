package wol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/processutil"
)

type neighbor struct {
	IP, MAC, Device string
}

// LocalTarget verifies standalone-host LAN membership using the OS neighbor
// table. Exact registered IP/MAC and local interface matches are required;
// matching private subnets alone never suffice. Derived edges stay local.
func LocalTarget(ctx context.Context, target model.Node, nodes []model.Node) (model.Node, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if len(target.WakeInterfaces) == 0 {
		return model.Node{}, errors.New("target has no retained wake interfaces")
	}
	local, err := Interfaces()
	if err != nil {
		return model.Node{}, err
	}
	devices, err := localDevices(local)
	if err != nil {
		return model.Node{}, err
	}
	table, err := readNeighbors(ctx)
	if err != nil {
		return model.Node{}, err
	}
	if derived, ok := localTarget(local, devices, table, target, nodes, time.Now()); ok {
		return derived, nil
	}
	// A cold ARP cache can learn an awake, already-verified witness without a
	// provider call or an execution service on this host. This is not a magic
	// packet and is never sent to the sleeping target.
	probes := 0
	for _, witness := range nodes {
		if !witness.Online || witness.Disabled || witness.ControlPending {
			continue
		}
		for _, edge := range target.LANPeers {
			if edge.ID != witness.ID || !CanSend(witness, target) || !slices.Contains(witness.WakeInterfaces, edge.Remote) {
				continue
			}
			for _, source := range local {
				if !SameSubnet(source, edge.Remote) || probes >= 3 {
					continue
				}
				probes++
				a, _, _ := parse(source)
				b, _, _ := parse(edge.Remote)
				_ = writeUDP(ctx, a.Addr(), b.Addr(), "9", []byte("control-lan-neighbor-v1"))
			}
		}
	}
	if probes != 0 {
		timer := time.NewTimer(150 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return model.Node{}, ctx.Err()
		case <-timer.C:
		}
		if table, err = readNeighbors(ctx); err == nil {
			if derived, ok := localTarget(local, devices, table, target, nodes, time.Now()); ok {
				return derived, nil
			}
		}
	}
	return model.Node{}, errors.New("no matching local IP/MAC neighbor for the target or a verified LAN witness")
}

func localDevices(local []model.WakeInterface) (map[model.WakeInterface]string, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	result := map[model.WakeInterface]string{}
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		for _, address := range addresses {
			nic := model.WakeInterface{Address: address.String(), MAC: iface.HardwareAddr.String()}
			if slices.Contains(local, nic) {
				result[nic] = iface.Name
			}
		}
	}
	return result, nil
}

func localTarget(local []model.WakeInterface, devices map[model.WakeInterface]string, table []neighbor, target model.Node, nodes []model.Node, now time.Time) (model.Node, bool) {
	derived := target
	derived.LANPeers = nil
	for _, source := range local {
		device := devices[source]
		if device == "" {
			continue
		}
		for _, remote := range target.WakeInterfaces {
			if !SameSubnet(source, remote) {
				continue
			}
			verified := now
			matched := hasNeighbor(table, device, source, remote)
			if !matched {
				for _, edge := range target.LANPeers {
					if edge.Local != remote || edge.VerifiedAt.After(now) || now.Sub(edge.VerifiedAt) >= model.LANPeerTTL {
						continue
					}
					for _, witness := range nodes {
						if witness.ID == edge.ID && witness.Online && slices.Contains(witness.WakeInterfaces, edge.Remote) && SameSubnet(source, edge.Remote) && hasNeighbor(table, device, source, edge.Remote) {
							matched = true
							verified = edge.VerifiedAt
							break
						}
					}
					if matched {
						break
					}
				}
			}
			if matched {
				derived.LANPeers = append(derived.LANPeers, model.LANPeer{ID: "local", Local: remote, Remote: source, VerifiedAt: verified})
			}
		}
	}
	return derived, len(derived.LANPeers) > 0
}

func hasNeighbor(table []neighbor, device string, local, remote model.WakeInterface) bool {
	ip, _, err := parse(remote)
	if err != nil {
		return false
	}
	localIP, _, err := parse(local)
	if err != nil {
		return false
	}
	for _, entry := range table {
		if entry.IP == ip.Addr().String() && normalizeMAC(entry.MAC) == normalizeMAC(remote.MAC) && (entry.Device == device || entry.Device == localIP.Addr().String()) {
			return true
		}
	}
	return false
}

func normalizeMAC(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ':' || r == '-' })
	if len(parts) != 6 {
		return ""
	}
	mac := make(net.HardwareAddr, 6)
	for i, part := range parts {
		v, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return ""
		}
		mac[i] = byte(v)
	}
	return mac.String()
}

type neighborOutput struct{ buffer bytes.Buffer }

func (b *neighborOutput) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > 1<<20 {
		return 0, errors.New("neighbor table exceeds 1 MiB")
	}
	return b.buffer.Write(data)
}

func neighborCommand(ctx context.Context, command string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	processutil.HideWindow(cmd)
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var output neighborOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return output.buffer.Bytes(), nil
}

func parseUnixNeighbors(data []byte) []neighbor {
	var result []neighbor
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		// BSD arp -an: ? (IP) at MAC on DEVICE ...
		if len(fields) >= 6 && fields[2] == "at" && fields[4] == "on" {
			ip := strings.Trim(fields[1], "()")
			if address, err := netip.ParseAddr(ip); err == nil && address.Is4() && normalizeMAC(fields[3]) != "" {
				result = append(result, neighbor{IP: ip, MAC: normalizeMAC(fields[3]), Device: fields[5]})
			}
		}
	}
	return result
}

func parseWindowsNeighbors(data []byte) []neighbor {
	var result []neighbor
	device := ""
	for line := range strings.SplitSeq(string(data), "\n") {
		fields := strings.Fields(line)
		if strings.Contains(line, "---") {
			device = ""
			for _, field := range fields {
				if ip, err := netip.ParseAddr(field); err == nil && ip.Is4() {
					device = field
					break
				}
			}
			continue
		}
		if len(fields) >= 2 && device != "" {
			if ip, err := netip.ParseAddr(fields[0]); err == nil && ip.Is4() && normalizeMAC(fields[1]) != "" {
				result = append(result, neighbor{IP: fields[0], MAC: normalizeMAC(fields[1]), Device: device})
			}
		}
	}
	return result
}

func parseLinuxNeighbors(data []byte) ([]neighbor, error) {
	var rows []struct {
		IP     string   `json:"dst"`
		MAC    string   `json:"lladdr"`
		Device string   `json:"dev"`
		State  []string `json:"state"`
	}
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, err
	}
	var result []neighbor
	for _, row := range rows {
		if ip, err := netip.ParseAddr(row.IP); err == nil && ip.Is4() && normalizeMAC(row.MAC) != "" && !slices.Contains(row.State, "FAILED") && !slices.Contains(row.State, "INCOMPLETE") {
			result = append(result, neighbor{IP: row.IP, MAC: normalizeMAC(row.MAC), Device: row.Device})
		}
	}
	return result, nil
}
