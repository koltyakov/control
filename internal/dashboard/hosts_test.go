package dashboard

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/koltyakov/control/internal/model"
)

func hybridSnapshots() []model.NodeActivitySnapshot {
	return []model.NodeActivitySnapshot{
		{ID: "user", Name: "host", HybridID: "pair", HybridRole: "user", Online: true, Status: "ready", OS: "linux",
			System:           &model.SystemInfo{CPUModel: "User sample", MemoryUsedBytes: 1, MemoryTotalBytes: 10},
			ConnectionRoutes: &model.ConnectionRouteCounts{Direct: 1}, Tunnels: &model.TunnelCounts{Forward: 1}, RetainedTunnels: &model.TunnelCounts{Reverse: 1}},
		{ID: "system", Name: "host-system", HybridID: "pair", HybridRole: "system", Online: true, Status: "ready", OS: "linux",
			System:           &model.SystemInfo{CPUModel: "System sample", MemoryUsedBytes: 2, MemoryTotalBytes: 10},
			ConnectionRoutes: &model.ConnectionRouteCounts{Relay: 2}, Tunnels: &model.TunnelCounts{Reverse: 2}, RetainedTunnels: &model.TunnelCounts{Forward: 2}},
	}
}

func TestHybridMachineRows(t *testing.T) {
	for _, test := range []struct {
		name, status, work, connections, metrics string
		online                                   bool
		change                                   func([]model.NodeActivitySnapshot)
	}{
		{"both online", "idle", "0", "1/0/2", "User sample", true, func([]model.NodeActivitySnapshot) {}},
		{"system fallback", "idle", "0", "0/0/2", "System sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].Online, n[0].Status = false, "offline"
		}},
		{"both offline", "offline", "-", "-", "User sample", false, func(n []model.NodeActivitySnapshot) {
			for i := range n {
				n[i].Online, n[i].Status = false, "offline"
			}
		}},
		{"system busy", "busy", "3", "1/0/2", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].ActiveCount, n[1].ActiveCount = 1, 2
		}},
		{"system reserved", "reserved", "0", "1/0/2", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[1].Leased = true
		}},
		{"busy user leased system", "reserved", "1", "1/0/2", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].ActiveCount, n[1].Leased = 1, true
		}},
		{"system update", "update", "-", "-", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[1].Online, n[1].Status = false, "update"
			n[0].Status = "summary"
		}},
		{"user unavailable", "idle", "?", "-", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].Status = "unavailable"
		}},
		{"system unavailable", "busy", "?", "-", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].ActiveCount, n[1].Status = 1, "unavailable"
		}},
		{"disabled system", "idle", "0", "1/0/2", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[1].Disabled = true
		}},
		{"both disabled", "disabled", "0", "1/0/2", "User sample", true, func(n []model.NodeActivitySnapshot) {
			n[0].Disabled, n[1].Disabled = true, true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes := hybridSnapshots()
			test.change(nodes)
			before, err := json.Marshal(nodes)
			if err != nil {
				t.Fatal(err)
			}
			rows := machineRows(nodes)
			if len(rows) != 1 {
				t.Fatalf("rows: %d", len(rows))
			}
			row := rows[0]
			if row.Name != "host" || row.ID != "user" || row.Online != test.online ||
				machineStatus(row.NodeActivitySnapshot, false) != test.status || row.work != test.work ||
				row.connections != test.connections || row.System.CPUModel != test.metrics {
				t.Fatalf("unexpected row: %+v, state=%s", row, machineStatus(row.NodeActivitySnapshot, false))
			}
			after, err := json.Marshal(nodes)
			if err != nil || string(before) != string(after) {
				t.Fatal("display projection changed the input snapshot", err)
			}
		})
	}
}

func TestHybridMachineRowsRequireUnambiguousMetadata(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func([]model.NodeActivitySnapshot) []model.NodeActivitySnapshot
	}{
		{"suffix only", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[0].HybridID, n[1].HybridID = "", ""
			return n
		}},
		{"different pairs", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[1].HybridID = "other"
			return n
		}},
		{"missing role", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[1].HybridRole = ""
			return n
		}},
		{"duplicate role", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[1].HybridRole = "user"
			return n
		}},
		{"same identity", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[1].ID = n[0].ID
			return n
		}},
		{"missing identity", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			n[1].ID = ""
			return n
		}},
		{"ambiguous pair", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot {
			extra := n[1]
			extra.ID = "extra"
			return append(n, extra)
		}},
		{"system only", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot { return n[1:] }},
		{"user only", func(n []model.NodeActivitySnapshot) []model.NodeActivitySnapshot { return n[:1] }},
	} {
		t.Run(test.name, func(t *testing.T) {
			nodes := test.change(hybridSnapshots())
			if rows := machineRows(nodes); len(rows) != len(nodes) {
				t.Fatalf("hid unpaired or ambiguous identities: %+v", rows)
			}
		})
	}
}

func TestHybridMachineRowCountsAndOrdering(t *testing.T) {
	nodes := hybridSnapshots()
	nodes[0].LastSeen = time.Now().Add(-time.Hour)
	nodes[1].LastSeen = time.Now()
	nodes[0].ActiveCount, nodes[1].ActiveCount = 1, 2
	nodes = append([]model.NodeActivitySnapshot{{Name: "Zebra"}, nodes[1], {Name: "Alpha"}}, nodes[0])
	rows := machineRows(nodes)
	if got := []string{rows[0].Name, rows[1].Name, rows[2].Name}; !reflect.DeepEqual(got, []string{"Alpha", "host", "Zebra"}) {
		t.Fatal(got)
	}
	host := rows[1]
	if host.ActiveCount != 3 || host.LastSeen != nodes[1].LastSeen ||
		*host.Tunnels != (model.TunnelCounts{Forward: 1, Reverse: 2}) ||
		*host.RetainedTunnels != (model.TunnelCounts{Forward: 2, Reverse: 1}) {
		t.Fatalf("lost host counts: %+v", host)
	}
	nodes[1].Tunnels, nodes[1].ConnectionRoutes = nil, nil
	nodes[1].DirectSessions = 1
	host = machineRows(nodes)[1]
	if host.Tunnels != nil || host.connections != "?/?/0" {
		t.Fatalf("missing readings became known: %+v", host)
	}
}

func TestRenderHybridHostPreservesContextDetail(t *testing.T) {
	now := time.Now()
	nodes := hybridSnapshots()
	nodes[1].ActiveCount = 1
	nodes[1].Active = []model.Activity{{ID: "system-job", Operation: "exec.run", State: "running", Started: now}}
	nodes[1].Recent = []model.Activity{{ID: "system-recent", Finished: now}}
	nodes[1].LeaseOwner, nodes[1].LeaseExpires = "owner", now.Add(time.Hour)
	nodes[1].Error = "system diagnostic"
	snapshot := model.PoolActivitySnapshot{ObservedAt: now, Nodes: nodes}
	text := Render(snapshot, now)
	machines, _, _ := strings.Cut(text, "  host-system leased by")
	if !strings.Contains(machines, "Machines  1 total · 1 online · 1 active") ||
		strings.Contains(machines, "host-system") || !strings.Contains(machines, "busy/leased") {
		t.Fatal("machine table did not group the host", text)
	}
	for _, want := range []string{"host-system leased by owner", "system diagnostic", "system-job", "system-recent", "User sample"} {
		if !strings.Contains(text, want) {
			t.Fatal("lost context detail", want, text)
		}
	}
	if strings.Contains(text, "System sample") {
		t.Fatal("duplicated hardware for the same host", text)
	}
	nodes[0].Online, nodes[0].Status = false, "offline"
	text = Render(snapshot, now)
	if !strings.Contains(text, "System sample") || strings.Contains(text, "User sample") {
		t.Fatal("hardware did not follow the online context", text)
	}
}

func TestRenderOfflineHybridHostHidesCachedUsage(t *testing.T) {
	now := time.Now()
	nodes := hybridSnapshots()
	for i := range nodes {
		nodes[i].Online, nodes[i].Status = false, "offline"
		nodes[i].System.Disks = []model.DiskUsage{{Path: "work", UsedBytes: 1 << 30, FreeBytes: 3 << 30, TotalBytes: 4 << 30}}
	}
	nodes[0].LastSeen, nodes[1].LastSeen = now.Add(-time.Hour), now.Add(-time.Minute)
	text := Render(model.PoolActivitySnapshot{ObservedAt: now, Nodes: nodes}, now)
	for _, want := range []string{"1 total · 0 online", "offline", "1m", "User sample", "work: 4.0GiB total", "↑2 ↓1"} {
		if !strings.Contains(text, want) {
			t.Fatal("missing offline host detail", want, text)
		}
	}
	for _, unwanted := range []string{"host-system", "System sample", "1.0GiB used", "3.0GiB", "1B/10B", "2B/10B"} {
		if strings.Contains(text, unwanted) {
			t.Fatal("duplicated context or exposed cached usage", unwanted, text)
		}
	}
}

func TestHybridManagementKeepsExplicitSystemTarget(t *testing.T) {
	calls := 0
	m := view{ctx: context.Background(), snapshot: model.PoolActivitySnapshot{Nodes: hybridSnapshots()}, options: Options{
		Manage: func(_ context.Context, id, action string) error {
			calls++
			if id != "system" || action != "disable" {
				t.Fatalf("wrong management target: %s %s", id, action)
			}
			return nil
		},
	}}
	apply := func(key rune) tea.Cmd {
		next, cmd := m.Update(tea.KeyPressMsg{Code: key, Text: string(key)})
		m = next.(view)
		return cmd
	}
	apply('m')
	if len(m.manager.nodes) != 2 {
		t.Fatal("grouping removed an explicit management target")
	}
	apply('j')
	next, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = next.(view)
	cmd := apply('d')
	if cmd == nil {
		t.Fatal("no management request")
	}
	m.Update(cmd())
	if calls != 1 {
		t.Fatal("management request did not execute exactly once", calls)
	}
}
