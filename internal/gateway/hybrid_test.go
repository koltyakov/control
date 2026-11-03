package gateway

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
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

// The original command must still work when a context fails after redemption.
func TestHybridDownloadsSurviveRedemptionUntilCompletion(t *testing.T) {
	g, server := installationFixture(t)
	links := hybridInvite(t, g, server)
	status := func(path string) int {
		t.Helper()
		response, err := http.Get(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode
	}
	for index := range links {
		id, credential, _ := hybridRedeem(t, links[index])
		if index == 0 {
			for _, suffix := range []string{"", "/info", "/binary"} {
				if got := status(links[0].URL + suffix); got != http.StatusOK {
					t.Fatalf("redeemed pending pair %q: %d", suffix, got)
				}
			}
			hybridConnect(t, server, links[0], id, credential, true)
			if got := status(links[0].URL); got != http.StatusOK {
				t.Fatalf("one registered context ended recovery: %d", got)
			}
			continue
		}
		hybridConnect(t, server, links[1], id, credential, true)
	}
	for _, link := range links {
		for _, suffix := range []string{"", "/info", "/binary"} {
			if got := status(link.URL + suffix); got != http.StatusGone {
				t.Fatalf("completed pair still serves %q: %d", suffix, got)
			}
		}
	}
}

func TestInstallationCheckDoesNotConsumeTicket(t *testing.T) {
	g, server := installationFixture(t)
	ctx := context.Background()
	q := enrollment.Request{AutoName: true, OS: "linux", Arch: "amd64", Gateway: server.URL, ServiceMode: "hybrid"}
	admin := testAdmin{URL: server.URL, Key: superKey}
	var link enrollment.Link
	if err := admin.JSON(ctx, "POST", "/v1/fleet/installations", q, &link); err != nil {
		t.Fatal(err)
	}
	ticket := testAdmin{URL: link.URL}
	if err := ticket.JSON(ctx, "POST", "/check", enrollment.Redemption{Name: "fresh"}, nil); err != nil {
		t.Fatal("new identity rejected", err)
	}
	g.mu.Lock()
	for _, i := range g.installations {
		if i.RedeemedID != "" || i.Name != "" {
			t.Error("check changed the invitation", i)
		}
	}
	g.mu.Unlock()
	worker, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, server.URL, commonKey, "taken", legacyUser, worker) == nil {
		t.Fatal("registration failed")
	}
	if err = ticket.JSON(ctx, "POST", "/check", enrollment.Redemption{Name: "taken"}, nil); err == nil {
		t.Fatal("another identity's registered name accepted")
	}
	// Reinstalling the same machine keeps its name when it proves its identity.
	proof := enrollment.Redemption{Name: "taken", PublicKey: worker.Public}
	proof.Signature = ed25519.Sign(worker.Private, enrollment.CheckMessage(link.URL[strings.LastIndex(link.URL, "/")+1:], proof))
	if err = ticket.JSON(ctx, "POST", "/check", proof, nil); err != nil {
		t.Fatal("same identity rejected", err)
	}
	unsigned := enrollment.Redemption{Name: "taken", PublicKey: worker.Public}
	if err = ticket.JSON(ctx, "POST", "/check", unsigned, nil); err == nil {
		t.Fatal("unsigned identity accepted")
	}
	// A taken companion name would otherwise fail after System installation.
	other, err := identity.Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if rawPeer(t, server.URL, commonKey, "host-system", legacyUser, other) == nil {
		t.Fatal("registration failed")
	}
	if err = ticket.JSON(ctx, "POST", "/check", enrollment.Redemption{Name: "host"}, nil); err == nil {
		t.Fatal("taken companion name accepted")
	}
	// A preflight proof must not be usable as a redemption proof.
	replayed := enrollment.Redemption{Name: "taken", PublicKey: worker.Public, CredentialHash: enrollment.Hash("credential"), Signature: proof.Signature}
	if err = ticket.JSON(ctx, "POST", "/redeem", replayed, nil); err == nil {
		t.Fatal("preflight proof redeemed the invitation")
	}
}

func TestLingerRequiresLinuxHybrid(t *testing.T) {
	_, server := installationFixture(t)
	admin := testAdmin{URL: server.URL, Key: superKey}
	for _, q := range []enrollment.Request{
		{Name: "a", OS: "linux", Arch: "amd64", ServiceMode: "user", Linger: true},
		{Name: "b", OS: "darwin", Arch: "arm64", ServiceMode: "hybrid", Linger: true},
	} {
		q.Gateway = server.URL
		if err := admin.JSON(context.Background(), "POST", "/v1/fleet/installations", q, nil); err == nil {
			t.Fatal("linger accepted", q)
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
