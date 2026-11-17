package gateway

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func hostRenameFixture(t *testing.T) (*Gateway, *httptest.Server, testBrowser, testAdmin, []model.Node) {
	t.Helper()
	g, server := installationFixture(t)
	owner, admin := createTestUser(t, server, "host-owner")
	provisionBrowser(t, admin, "")
	browser := browserLogin(t, server.URL, owner.Name, browserPassword)
	var nodes []model.Node
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, role := range []string{"user", "system"} {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		// Deliberately mismatched aliases model hosts renamed before paired rename.
		n := model.Node{ID: id.ID, UserID: owner.ID, PublicKey: id.Public, Name: role + "-old", HybridID: "pair", HybridRole: role}
		nodes = append(nodes, n)
		g.owners[id.ID], g.nodes[id.ID] = owner.ID, n
	}
	g.machineStates[nodes[0].ID] = model.MachineState{Disabled: true, Revision: 7}
	if err := g.persist(); err != nil {
		t.Fatal(err)
	}
	return g, server, browser, admin, nodes
}

func assertHostNames(t *testing.T, g *Gateway, nodes []model.Node, names []string) {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	stored := map[string]model.Node{}
	if err := readRecords(g.db, "nodes", "id", stored); err != nil {
		t.Fatal(err)
	}
	for index, n := range nodes {
		if g.nodes[n.ID].Name != names[index] || stored[n.ID].Name != names[index] {
			t.Fatalf("context %s: memory=%q disk=%q want=%q", n.HybridRole, g.nodes[n.ID].Name, stored[n.ID].Name, names[index])
		}
	}
}

func TestConsoleHybridHostRename(t *testing.T) {
	g, server, browser, _, nodes := hostRenameFixture(t)
	// Offline contexts do not need a running node. Existing mismatched aliases
	// are repaired even when System no longer has its original name.
	for _, name := range []string{"vm.host", strings.Repeat("a", 56)} {
		data, _ := browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[0].ID,
			map[string]any{"name": name}, 200)
		var state model.MachineState
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatal(err)
		}
		want := []string{name, name + "-system"}
		if state.Name != want[0] {
			t.Fatal("response did not describe selected context", state)
		}
		assertHostNames(t, g, nodes, want)
		g.mu.Lock()
		if g.machineStates[nodes[0].ID].Revision != 7 || !g.machineStates[nodes[0].ID].Disabled ||
			g.machineStates[nodes[1].ID].Revision != 0 || g.nodes[nodes[1].ID].HybridID != "pair" {
			t.Error("rename changed policy or pairing")
		}
		g.mu.Unlock()
	}
	// Explicit System renames remain context-only.
	browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[1].ID, map[string]string{"name": "only-system"}, 200)
	assertHostNames(t, g, nodes, []string{strings.Repeat("a", 56), "only-system"})
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	server.Close()
	reloaded, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reloaded.Close() }()
	assertHostNames(t, reloaded, nodes, []string{strings.Repeat("a", 56), "only-system"})
}

func TestConsoleHybridHostRenameConflicts(t *testing.T) {
	for _, conflict := range []string{"user-invitation", "system-invitation", "registered", "redeemed", "client", "too-long", "ambiguous"} {
		t.Run(conflict, func(t *testing.T) {
			g, _, browser, _, nodes := hostRenameFixture(t)
			name, status := "new", 409
			body := map[string]any{"name": name, "force": true}
			g.mu.Lock()
			switch conflict {
			case "user-invitation", "system-invitation", "redeemed":
				reserved := name
				if conflict != "user-invitation" {
					reserved += "-system"
				}
				i := installation{Invitation: enrollment.Invitation{ID: "reserved", UserID: nodes[0].UserID, Name: reserved, ExpiresAt: time.Now().Add(time.Hour)}}
				if conflict == "redeemed" {
					i.RedeemedID = "other"
				}
				g.installations["reserved"] = i
				body["force"] = false
			case "registered":
				g.nodes["other"] = model.Node{ID: "other", UserID: nodes[0].UserID, Name: name + "-system"}
				g.installations["reserved"] = installation{Invitation: enrollment.Invitation{
					ID: "reserved", UserID: nodes[0].UserID, Name: name, ExpiresAt: time.Now().Add(time.Hour),
				}}
			case "client":
				g.clientOwners["1234567890123456-other"] = true
				g.owners["1234567890123456-other"] = nodes[0].UserID
				body["name"] = "cli-1234567890123456"
			case "too-long":
				body["name"], status = strings.Repeat("a", 57), 400
			case "ambiguous":
				other := nodes[1]
				other.ID = "duplicate"
				g.nodes[other.ID] = other
			}
			g.mu.Unlock()
			browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[0].ID, body, status)
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.nodes[nodes[0].ID].Name != nodes[0].Name || g.machineStates[nodes[0].ID].Name != "" ||
				g.installations["reserved"].Revoked {
				t.Fatal("conflict changed a name, policy or reservation")
			}
			if n, ok := g.nodes[nodes[1].ID]; ok && n.Name != nodes[1].Name {
				t.Fatal("conflict renamed System")
			}
		})
	}
}

func TestConsoleHybridHostRenameForceAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			g, _, browser, _, nodes := hostRenameFixture(t)
			g.mu.Lock()
			for _, name := range []string{"new", "new-system"} {
				g.installations[name] = installation{Invitation: enrollment.Invitation{
					ID: name, Name: name, UserID: nodes[0].UserID,
					ExpiresAt: time.Now().Add(time.Hour),
				}}
			}
			if err := g.persist(); err != nil {
				g.mu.Unlock()
				t.Fatal(err)
			}
			if fail {
				// Fail after the transaction has begun, not just before Begin.
				if _, err := g.db.Exec(`CREATE TRIGGER reject_host_rename BEFORE UPDATE ON nodes WHEN NEW.name='new-system' BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
					g.mu.Unlock()
					t.Fatal(err)
				}
			}
			g.mu.Unlock()
			status := 200
			if fail {
				status = 500
			}
			browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[0].ID,
				map[string]any{"name": "new", "force": true}, status)
			want := []string{"new", "new-system"}
			if fail {
				want = []string{nodes[0].Name, nodes[1].Name}
			}
			assertHostNames(t, g, nodes, want)
			g.mu.Lock()
			defer g.mu.Unlock()
			stored := map[string]installation{}
			if err := readRecords(g.db, "invitations", "hash", stored); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"new", "new-system"} {
				if g.installations[name].Revoked != !fail || stored[name].Revoked != !fail {
					t.Fatal("invitation revocation was not atomic with both names")
				}
			}
			var count int
			if err := g.db.QueryRow(`SELECT count(*) FROM audit_events WHERE json_extract(data,'$.action')='machine.rename'`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if (!fail && count != 2) || (fail && count != 0) {
				t.Fatal("audit changes were not atomic", count)
			}
			if fail {
				if _, ok := g.machineStates[nodes[1].ID]; ok {
					t.Fatal("rollback left a previously absent policy")
				}
			}
		})
	}
}

func TestHybridRenameUsesOnlySameFleetPairing(t *testing.T) {
	for _, scenario := range []string{"unpaired", "foreign", "missing", "bearer"} {
		t.Run(scenario, func(t *testing.T) {
			g, server, browser, admin, nodes := hostRenameFixture(t)
			g.mu.Lock()
			switch scenario {
			case "unpaired":
				n := g.nodes[nodes[0].ID]
				n.HybridID = ""
				g.nodes[n.ID] = n
				nodes[1].Name = n.Name + "-system"
				g.nodes[nodes[1].ID] = nodes[1]
			case "foreign":
				// Reusing Hybrid metadata in another fleet must not establish
				// pairing. Permanent identity ownership remains unchanged.
				id, err := identity.Generate()
				if err != nil {
					g.mu.Unlock()
					t.Fatal(err)
				}
				n := g.nodes[nodes[1].ID]
				delete(g.nodes, n.ID)
				n.ID, n.PublicKey, n.UserID = id.ID, id.Public, legacyUser
				nodes[1] = n
				g.owners[n.ID], g.nodes[n.ID] = legacyUser, n
			case "missing":
				delete(g.nodes, nodes[1].ID)
			}
			if err := g.persist(); err != nil {
				g.mu.Unlock()
				t.Fatal(err)
			}
			g.mu.Unlock()
			if scenario == "bearer" {
				// All bearer clients use this exact endpoint, without a web flag.
				_, foreign := createTestUser(t, server, "other")
				if err := foreign.JSON(t.Context(), "PATCH", "/v1/fleet/nodes/"+nodes[0].ID, map[string]string{"name": "stolen"}, nil); err == nil {
					t.Fatal("foreign account renamed a host")
				}
				if err := admin.JSON(t.Context(), "PATCH", "/v1/fleet/nodes/"+nodes[0].ID, map[string]string{"name": "new"}, nil); err != nil {
					t.Fatal(err)
				}
			} else {
				browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[0].ID, map[string]string{"name": "new"}, 200)
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.nodes[nodes[0].ID].Name != "new" {
				t.Fatal("User rename failed")
			}
			if system, ok := g.nodes[nodes[1].ID]; ok {
				want := nodes[1].Name
				if scenario == "bearer" {
					want = "new-system"
				}
				if system.Name != want {
					t.Fatal("rename used a suffix or foreign pairing", system)
				}
			}
		})
	}
}

func TestHybridRenameNotifiesBothContextsAfterCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback"}[fail], func(t *testing.T) {
			g, _, browser, _, nodes := hostRenameFixture(t)
			user := renamePacketPeer(t, g, nodes[0].ID, nodes[0].UserID)
			system := renamePacketPeer(t, g, nodes[1].ID, nodes[1].UserID)
			foreign := renamePacketPeer(t, g, "foreign", "other-owner")
			g.mu.Lock()
			user.machineStatePush, system.machineStatePush = true, true
			if fail {
				if _, err := g.db.Exec(`CREATE TRIGGER reject_host_rename BEFORE UPDATE ON nodes BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
					g.mu.Unlock()
					t.Fatal(err)
				}
			}
			g.mu.Unlock()
			status := 200
			if fail {
				status = 500
			}
			browser.request(t, "PATCH", "/console/api/fleet/nodes/"+nodes[0].ID, map[string]string{"name": "new"}, status)
			for _, peer := range []*connection{user, system} {
				if fail {
					if len(peer.out) != 0 {
						t.Fatal("failed rename published a notification")
					}
					continue
				}
				for _, kind := range []string{"machine.state", "directory.changed"} {
					select {
					case packet := <-peer.out:
						if packet.Kind != kind {
							t.Fatal("unexpected rename notification", packet.Kind)
						}
						peer.releasePacket(packet)
					default:
						t.Fatal("missing rename notification", kind)
					}
				}
			}
			if len(foreign.out) != 0 {
				t.Fatal("notification crossed fleet boundary")
			}
		})
	}
}
