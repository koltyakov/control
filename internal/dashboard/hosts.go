package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/koltyakov/control/internal/model"
)

// machineRow is a display projection, never an execution or management target.
type machineRow struct {
	model.NodeActivitySnapshot
	work, connections string
	observed          bool
}

func machineRows(nodes []model.NodeActivitySnapshot) []machineRow {
	pairs := map[string][]int{}
	for i, n := range nodes {
		if n.HybridID != "" {
			pairs[n.HybridID] = append(pairs[n.HybridID], i)
		}
	}
	companions := map[int]int{}
	for _, indexes := range pairs {
		if len(indexes) != 2 {
			continue
		}
		a, b := indexes[0], indexes[1]
		if nodes[a].HybridRole == "system" {
			a, b = b, a
		}
		if nodes[a].HybridRole == "user" && nodes[b].HybridRole == "system" &&
			nodes[a].ID != "" && nodes[b].ID != "" && nodes[a].ID != nodes[b].ID {
			companions[a], companions[b] = b, a
		}
	}
	rows := make([]machineRow, 0, len(nodes))
	for i, n := range nodes {
		if other, ok := companions[i]; ok {
			if n.HybridRole == "system" {
				continue
			}
			rows = append(rows, hybridRow(n, nodes[other]))
		} else {
			rows = append(rows, singleMachineRow(n))
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i].Name, rows[j].Name
		if strings.EqualFold(a, b) {
			return a < b
		}
		return strings.ToLower(a) < strings.ToLower(b)
	})
	return rows
}

func singleMachineRow(n model.NodeActivitySnapshot) machineRow {
	observed := n.Status == "ready" || n.Status == "summary"
	work := "?"
	if observed {
		work = fmt.Sprint(n.ActiveCount)
	}
	if n.Status == "online" || n.Status == "update" || !n.Online {
		work = "-"
	}
	connections := "-"
	if n.Status == "ready" {
		connections = connectionRatio(n)
	}
	return machineRow{n, work, connections, observed}
}

func hybridRow(user, system model.NodeActivitySnapshot) machineRow {
	n := user
	if machinePriority(system) > machinePriority(user) {
		n = system
	}
	n.ID, n.Name = user.ID, user.Name
	n.Online = user.Online || system.Online
	if system.LastSeen.After(user.LastSeen) {
		n.LastSeen = system.LastSeen
	} else {
		n.LastSeen = user.LastSeen
	}
	metrics := user
	if !user.Online && system.Online || !n.Online && user.System == nil {
		metrics = system
	}
	n.System, n.OS, n.Software = metrics.System, metrics.OS, metrics.Software
	n.ActiveCount, n.Leased = 0, false
	n.Tunnels, n.RetainedTunnels = nil, nil
	routes := &model.ConnectionRouteCounts{}
	workKnown, routesKnown, tunnelsKnown, observed := true, true, true, false
	for _, context := range []model.NodeActivitySnapshot{user, system} {
		n.RetainedTunnels = addTunnels(n.RetainedTunnels, context.RetainedTunnels)
		if !context.Online {
			continue
		}
		fresh := context.Status == "ready" || context.Status == "summary"
		observed = observed || fresh
		workKnown = workKnown && fresh
		n.ActiveCount += context.ActiveCount
		n.Leased = n.Leased || context.Leased || context.LeaseOwner != ""
		routesKnown = routesKnown && context.Status == "ready"
		if r := context.ConnectionRoutes; r != nil {
			routes.Direct += r.Direct
			routes.TURN += r.TURN
			routes.Relay += r.Relay
			routes.Unknown += r.Unknown
		} else {
			routes.Unknown += context.DirectSessions
			routes.Relay += context.RelaySessions
		}
		tunnelsKnown = tunnelsKnown && context.Tunnels != nil
		n.Tunnels = addTunnels(n.Tunnels, context.Tunnels)
	}
	if !tunnelsKnown {
		n.Tunnels = nil
	}
	row := singleMachineRow(n)
	row.observed = observed
	if n.Online && n.Status != "update" && !workKnown {
		row.work = "?"
	}
	row.connections = "-"
	if n.Online && routesKnown {
		n.ConnectionRoutes = routes
		row.connections = connectionRatio(n)
	}
	return row
}

func addTunnels(a, b *model.TunnelCounts) *model.TunnelCounts {
	if b == nil {
		return a
	}
	if a == nil {
		a = &model.TunnelCounts{}
	}
	a.Forward += b.Forward
	a.Reverse += b.Reverse
	return a
}

func machinePriority(n model.NodeActivitySnapshot) int {
	if n.Status == "update" {
		return 8
	}
	if !n.Online {
		return 0
	}
	switch machineStatus(n, false) {
	case "busy":
		return 7
	case "reserved":
		return 6
	case "enabling", "disabling":
		return 5
	case "idle":
		return 4
	case "disabled":
		return 3
	case "unavailable":
		return 2
	default:
		return 1
	}
}

func machineStatus(n model.NodeActivitySnapshot, details bool) string {
	status := n.Status
	if n.Status == "ready" || n.Status == "summary" {
		status = "idle"
		if n.ActiveCount > 0 {
			status = "busy"
		}
		if n.LeaseOwner != "" || n.Leased {
			if details {
				status += "/leased"
			} else {
				status = "reserved"
			}
		}
	}
	if n.Disabled {
		status = "disabled"
	}
	if n.ControlPending {
		status = "enabling"
		if n.Disabled {
			status = "disabling"
		}
	}
	if n.Status == "update" {
		status = "update"
	}
	return status
}
