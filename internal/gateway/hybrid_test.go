package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

func TestHybridInvitationPair(t *testing.T) {
	g, server := installationFixture(t)
	admin := testAdmin{URL: server.URL, Key: superKey}
	ctx := context.Background()
	var link enrollment.Link
	q := enrollment.Request{Name: "hybrid", OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "hybrid"}
	if err := admin.JSON(ctx, "POST", "/v1/fleet/installations", q, &link); err != nil {
		t.Fatal(err)
	}
	if link.ServiceMode != "hybrid" || link.HybridRole != "user" || link.HybridID != link.ID || strings.Contains(link.Command, "sudo bash") {
		t.Fatal("incorrect User-first Hybrid installation", link)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	if len(ticket) != 8 {
		t.Fatal("Hybrid command did not use an 8-character ticket")
	}
	hash := enrollment.Hash(enrollment.SystemTicket(ticket))
	g.mu.Lock()
	system, ok := g.installations[hash]
	persisted := map[string]installation{}
	err := readRecords(g.db, "invitations", "hash", persisted)
	g.mu.Unlock()
	if !ok || system.Name != "hybrid-system" || system.HybridID != link.ID || system.HybridRole != "system" || system.ServiceMode != "system" || err != nil || len(persisted) != 2 {
		t.Fatal("companion was not reserved and committed with User", system, persisted, err)
	}
	// A collision cannot leave half of a new installation behind.
	q.Name, q.ServiceMode = "hybrid-system", "user"
	if err = admin.JSON(ctx, "POST", "/v1/fleet/installations", q, nil); err == nil {
		t.Fatal("claimed companion reservation")
	}
	q.Name, q.ServiceMode, q.Force = "hybrid", "hybrid", true
	var replacement enrollment.Link
	if err = admin.JSON(ctx, "POST", "/v1/fleet/installations", q, &replacement); err != nil {
		t.Fatal("force replacement", err)
	}
	g.mu.Lock()
	oldUser := g.installations[enrollment.Hash(ticket)]
	oldSystem := g.installations[hash]
	g.mu.Unlock()
	if !oldUser.Revoked || !oldSystem.Revoked {
		t.Fatal("force left a usable companion ticket")
	}
	if err = admin.JSON(ctx, "DELETE", "/v1/fleet/installations/"+replacement.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, i := range g.installations {
		if i.HybridID == replacement.ID && !i.Revoked {
			t.Fatal("Hybrid revocation left a usable context")
		}
	}
}

func TestHybridInvitationRollback(t *testing.T) {
	for _, operation := range []string{"create", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			g, server := installationFixture(t)
			admin := testAdmin{URL: server.URL, Key: superKey}
			ctx := context.Background()
			q := enrollment.Request{Name: "rollback", OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "hybrid"}
			var link enrollment.Link
			if operation == "revoke" {
				if err := admin.JSON(ctx, "POST", "/v1/fleet/installations", q, &link); err != nil {
					t.Fatal(err)
				}
			}
			if err := g.db.Close(); err != nil {
				t.Fatal(err)
			}
			var err error
			if operation == "create" {
				err = admin.JSON(ctx, "POST", "/v1/fleet/installations", q, nil)
			} else {
				err = admin.JSON(ctx, "DELETE", "/v1/fleet/installations/"+link.ID, nil, nil)
			}
			if err == nil {
				t.Fatal("acknowledged failed persistence")
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if operation == "create" && len(g.installations) != 0 {
				t.Fatal("failed creation left half a pair")
			}
			for _, invitation := range g.installations {
				if invitation.Revoked {
					t.Fatal("failed revocation changed live pair")
				}
			}
		})
	}
}

func TestHybridRegistrationRequiresInstructionOnlySystem(t *testing.T) {
	g, _ := installationFixture(t)
	i := installation{Invitation: enrollment.Invitation{UserID: legacyUser, ID: "companion", HybridRole: "system", Name: "worker-system", RedeemedID: "worker"}}
	i.Asset.OS, i.Asset.Arch = "linux", "amd64"
	g.mu.Lock()
	defer g.mu.Unlock()
	g.installations["companion"] = i
	n := model.Node{UserID: legacyUser, ID: "worker", Name: "worker-system", OS: "linux", Software: buildinfo.Info{Arch: "amd64"}}
	if g.allowInstallation("install:companion", n) {
		t.Fatal("companion omitted its execution guard")
	}
	n.InstructionOnly, n.RunAs = true, "user"
	if g.allowInstallation("install:companion", n) {
		t.Fatal("companion ran as User")
	}
	n.RunAs = "system"
	if !g.allowInstallation("install:companion", n) {
		t.Fatal("valid System companion rejected")
	}
}
