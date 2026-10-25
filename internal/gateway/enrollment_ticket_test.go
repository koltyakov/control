package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/enrollment"
)

func TestInstallationTicketCollisions(t *testing.T) {
	first := make([]byte, 6)
	second := bytes.Repeat([]byte{255}, 6)
	firstTicket := base64.RawURLEncoding.EncodeToString(first)
	secondTicket := base64.RawURLEncoding.EncodeToString(second)
	for _, companion := range []bool{false, true} {
		t.Run(map[bool]string{false: "ticket", true: "companion"}[companion], func(t *testing.T) {
			hash := enrollment.Hash(firstTicket)
			if companion {
				hash = enrollment.Hash(enrollment.SystemTicket(firstTicket))
			}
			previous := installation{Invitation: enrollment.Invitation{UserID: "another-fleet", ID: "retained"}}
			g := &Gateway{installations: map[string]installation{hash: previous}}
			source := io.MultiReader(bytes.NewReader(first), bytes.NewReader(second))
			ticket, err := g.newInstallationTicket(source, companion)
			if err != nil || ticket != secondTicket {
				t.Fatalf("collision was not skipped: %q, %v", ticket, err)
			}
			if ticket, err = g.newInstallationTicket(bytes.NewReader(bytes.Repeat(first, 8)), companion); err == nil || ticket != "" {
				t.Fatal("repeated collisions did not fail")
			}
			if ticket, err = g.newInstallationTicket(bytes.NewReader(first[:5]), companion); err == nil || ticket != "" {
				t.Fatal("incomplete randomness did not fail")
			}
			if len(g.installations) != 1 || g.installations[hash].ID != previous.ID || g.installations[hash].UserID != previous.UserID {
				t.Fatal("ticket generation changed an existing invitation")
			}
		})
	}
}

func TestLegacyInstallationTicket(t *testing.T) {
	g, server := installationFixture(t)
	link := invite(t, server, "legacy-ticket")
	shortTicket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	legacyTicket := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{42}, 32))
	oldHash, legacyHash := enrollment.Hash(shortTicket), enrollment.Hash(legacyTicket)
	g.mu.Lock()
	g.installations[legacyHash] = g.installations[oldHash]
	delete(g.installations, oldHash)
	err := g.commit(change{invitations: []string{oldHash, legacyHash}})
	g.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	link.URL = server.URL + "/install/" + legacyTicket
	for _, suffix := range []string{"", "/info", "/binary"} {
		resp, err := http.Get(link.URL + suffix)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("legacy ticket GET %s: %d", suffix, resp.StatusCode)
		}
	}
	proof, credential := redeemRequest(t, link)
	for range 2 {
		if err := (testAdmin{URL: link.URL}).JSON(context.Background(), "POST", "/redeem", proof, nil); err != nil {
			t.Fatal("legacy redemption or response recovery failed", err)
		}
	}
	if role, _ := g.role(credential); role != "common" {
		t.Fatal("legacy ticket did not bind the node credential")
	}
}
