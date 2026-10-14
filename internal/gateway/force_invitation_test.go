package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

func TestForceRetractsPendingInvitationAtomically(t *testing.T) {
	for _, action := range []string{"add", "rename"} {
		for _, failCommit := range []bool{false, true} {
			t.Run(action+"/failCommit="+map[bool]string{false: "false", true: "true"}[failCommit], func(t *testing.T) {
				g, s := installationFixture(t)
				ctx := context.Background()
				a := testAdmin{URL: s.URL, Key: superKey}
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				g.mu.Lock()
				g.owners[id.ID] = legacyUser
				g.nodes[id.ID] = model.Node{ID: id.ID, PublicKey: id.Public, Name: "original", UserID: legacyUser}
				err = g.persist()
				g.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				old := invite(t, s, "reserved")
				proof, _ := redeemRequest(t, old)
				request := func(force bool) (enrollment.Link, error) {
					var link enrollment.Link
					if action == "add" {
						err := a.JSON(ctx, "POST", "/v1/fleet/installations", enrollment.Request{Name: "reserved", OS: "linux", Arch: "amd64", Gateway: s.URL, Force: force}, &link)
						return link, err
					}
					err := a.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, map[string]any{"name": "reserved", "force": force}, nil)
					return link, err
				}
				if _, err := request(false); err == nil {
					t.Fatal("expected pending-invitation conflict", err)
				}
				if failCommit {
					if err := g.db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				fresh, err := request(true)
				if (err != nil) != failCommit {
					t.Fatal("unexpected force result", err)
				}
				g.mu.Lock()
				oldHash := enrollment.Hash(old.URL[strings.LastIndex(old.URL, "/")+1:])
				pending := g.installations[oldHash]
				name := g.nodes[id.ID].Name
				count := len(g.installations)
				g.mu.Unlock()
				if pending.Revoked == failCommit {
					t.Fatal("revocation not committed or rolled back")
				}
				if failCommit {
					if name != "original" || count != 1 {
						t.Fatal("failed mutation changed registration or invitation registry", name, count)
					}
					assertInvitationGETStatus(t, old, http.StatusOK)
					return
				}
				assertInvitationGETStatus(t, old, http.StatusGone)
				if err := (testAdmin{URL: old.URL}).JSON(ctx, "POST", "/redeem", proof, nil); err == nil {
					t.Fatal("retracted URL redeemed")
				}
				if action == "rename" && name != "reserved" {
					t.Fatal("force did not rename", name)
				}
				if action == "add" {
					assertInvitationGETStatus(t, fresh, http.StatusOK)
					freshProof, _ := redeemRequest(t, fresh)
					if err := (testAdmin{URL: fresh.URL}).JSON(ctx, "POST", "/redeem", freshProof, nil); err != nil {
						t.Fatal("replacement invitation unusable", err)
					}
				}
				_ = g.Close()
				s.Close()
				reloaded, err := New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = reloaded.Close() }()
				s = httptest.NewServer(reloaded.Handler())
				defer s.Close()
				old.URL = s.URL + old.URL[strings.Index(old.URL, "/install/"):]
				assertInvitationGETStatus(t, old, http.StatusGone)
				if err := (testAdmin{URL: old.URL}).JSON(ctx, "POST", "/redeem", proof, nil); err == nil {
					t.Fatal("retracted invitation returned after restart")
				}
			})
		}
	}
}

func assertInvitationGETStatus(t *testing.T, link enrollment.Link, status int) {
	t.Helper()
	for _, suffix := range []string{"", "/info", "/binary"} {
		resp, err := http.Get(link.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != status {
			t.Fatalf("invitation %s returned %d, want %d", suffix, resp.StatusCode, status)
		}
	}
}

func TestForceInvitationPreservesFleetAndIdentityReservations(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	ctx := context.Background()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.owners[id.ID] = alice.ID
	g.nodes[id.ID] = model.Node{ID: id.ID, PublicKey: id.Public, UserID: alice.ID, Name: "original"}
	err = g.persist()
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	q := enrollment.Request{Name: "reserved", OS: "linux", Arch: "amd64", Gateway: s.URL}
	var own, foreign enrollment.Link
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", q, &own); err != nil {
		t.Fatal(err)
	}
	if err := b.JSON(ctx, "POST", "/v1/fleet/installations", q, &foreign); err != nil {
		t.Fatal(err)
	}
	path := "/v1/fleet/nodes/" + id.ID
	for _, unauthorized := range []testAdmin{b, {URL: s.URL, Key: commonKey}} {
		if err := unauthorized.JSON(ctx, "PATCH", path, map[string]any{"name": "reserved", "force": true}, nil); err == nil {
			t.Fatal("force bypassed authorization")
		}
	}
	if err := a.JSON(ctx, "PATCH", path, map[string]any{"name": "reserved", "force": true}, nil); err != nil {
		t.Fatal(err)
	}
	assertInvitationGETStatus(t, own, http.StatusGone)
	assertInvitationGETStatus(t, foreign, http.StatusOK)
	q.Force = true
	var foreignReplacement enrollment.Link
	if err := b.JSON(ctx, "POST", "/v1/fleet/installations", q, &foreignReplacement); err != nil {
		t.Fatal(err)
	}
	assertInvitationGETStatus(t, foreign, http.StatusGone)
	// Force add keeps the same-name existing-identity replacement contract.
	q.Force = true
	var replacement enrollment.Link
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", q, &replacement); err != nil || replacement.ReplaceID != id.ID {
		t.Fatal("force add lost identity reservation", replacement, err)
	}
	proof, _ := redeemRequest(t, replacement)
	if err := (testAdmin{URL: replacement.URL}).JSON(ctx, "POST", "/redeem", proof, nil); err == nil {
		t.Fatal("force invitation allowed identity takeover")
	}
	// A redeemed enrollment without a directory entry still cannot be stolen.
	q.Name, q.Force = "enrolled", false
	var enrolled enrollment.Link
	if err := a.JSON(ctx, "POST", "/v1/fleet/installations", q, &enrolled); err != nil {
		t.Fatal(err)
	}
	proof, _ = redeemRequest(t, enrolled)
	if err := (testAdmin{URL: enrolled.URL}).JSON(ctx, "POST", "/redeem", proof, nil); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(ctx, "PATCH", path, map[string]any{"name": "enrolled", "force": true}, nil); err == nil {
		t.Fatal("force stole redeemed enrollment")
	}
	if err := (testAdmin{URL: enrolled.URL}).JSON(ctx, "POST", "/redeem", proof, nil); err != nil {
		t.Fatal("force revoked redeemed recovery", err)
	}
}

func TestForceRenameDoesNotRetractInvitationsOnHardConflicts(t *testing.T) {
	for _, conflict := range []string{"registered", "client"} {
		t.Run(conflict, func(t *testing.T) {
			g, s := installationFixture(t)
			id, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			other, err := identity.Generate()
			if err != nil {
				t.Fatal(err)
			}
			name := "reserved"
			if conflict == "client" {
				name = "cli-" + other.ID[:16]
			}
			pending := invite(t, s, name)
			g.mu.Lock()
			g.owners[id.ID], g.owners[other.ID] = legacyUser, legacyUser
			g.nodes[id.ID] = model.Node{ID: id.ID, UserID: legacyUser, PublicKey: id.Public, Name: "original"}
			if conflict == "registered" {
				g.nodes[other.ID] = model.Node{ID: other.ID, UserID: legacyUser, PublicKey: other.Public, Name: name}
			} else {
				g.clientOwners[other.ID] = true
			}
			err = g.persist()
			g.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(context.Background(), "PATCH", "/v1/fleet/nodes/"+id.ID, map[string]any{"name": name, "force": true}, nil); err == nil {
				t.Fatal("force bypassed hard name conflict")
			}
			assertInvitationGETStatus(t, pending, http.StatusOK)
		})
	}
}
