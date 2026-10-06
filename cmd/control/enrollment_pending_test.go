package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

func TestNewInvitationReplacesPendingEnrollment(t *testing.T) {
	for _, stage := range []string{"before-config", "existing-profile", "replacement-identity", "replacement-profile-written", "replacement-retired"} {
		t.Run(stage, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "node.json")
			var infoRequests atomic.Int32
			var retiredID, reservedID string
			software := buildinfo.Current()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/install/new/info":
					infoRequests.Add(1)
					if r.URL.Query().Get("arch") != runtime.GOARCH {
						t.Error("installer did not select its architecture")
					}
					_ = json.NewEncoder(w).Encode(enrollment.Invitation{UserID: "fleet", Name: "replacement", ReplaceID: reservedID, Gateway: "http://" + r.Host, Asset: update.Asset{OS: software.OS, Arch: software.Arch, SHA256: software.SHA256}})
				case "/v1/node/state":
					var proof model.MachineStateProof
					if json.NewDecoder(r.Body).Decode(&proof) != nil || !proof.InspectOnly || !ed25519.Verify(proof.PublicKey, proof.Message(), proof.Signature) {
						http.Error(w, "invalid policy proof", http.StatusUnauthorized)
						return
					}
					_ = json.NewEncoder(w).Encode(model.MachineState{Unregistered: identity.ID(proof.PublicKey) == retiredID})
				default:
					http.Error(w, "old invitation is gone", http.StatusGone)
				}
			}))
			defer server.Close()
			previous := pendingEnrollment{URL: server.URL + "/install/old", Credential: "previous-token", Invitation: enrollment.Invitation{UserID: "fleet", Name: "old", Gateway: server.URL, ExpiresAt: time.Now().Add(-time.Hour)}}
			var cfg node.Config
			if stage != "before-config" {
				if err := createNodeConfig(config, "old", server.URL, "active-token", "127.0.0.1:7339", true); err != nil {
					t.Fatal(err)
				}
				var err error
				cfg, err = node.LoadConfig(config)
				if err != nil {
					t.Fatal(err)
				}
				if err = prepareReenrollment(config, &previous); err != nil {
					t.Fatal(err)
				}
				reservedID = previous.IdentityID
				if strings.HasPrefix(stage, "replacement-") {
					retiredID = previous.IdentityID
					if err = replaceRetiredIdentity(context.Background(), config, &previous); err != nil {
						t.Fatal(err)
					}
					reservedID = previous.IdentityID
					if stage == "replacement-profile-written" {
						if err = store.Bytes(config, previous.Config, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if stage == "replacement-retired" {
						retiredID, reservedID = previous.IdentityID, ""
					}
				}
			}
			path := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
			if err := store.Write(path, previous); err != nil {
				t.Fatal(err)
			}
			active, _ := os.ReadFile(config)
			pending, err := prepareEnrollment(context.Background(), config, server.URL+"/install/new", false)
			if err != nil {
				t.Fatal("new invitation was blocked by the previous attempt", err)
			}
			if pending.URL != server.URL+"/install/new" || pending.Credential == previous.Credential || pending.Credential == "" || pending.Invitation.Name != "replacement" {
				t.Fatal("new invitation did not replace pending enrollment")
			}
			if stage == "replacement-retired" {
				if pending.IdentityID == previous.IdentityID || pending.DataDir == previous.DataDir {
					t.Fatal("reused a retired replacement identity")
				}
			} else if pending.IdentityID != previous.IdentityID || pending.DataDir != previous.DataDir {
				t.Fatal("new invitation changed the pending machine identity")
			}
			if stage != "before-config" {
				var replacement node.Config
				if err = json.Unmarshal(pending.Config, &replacement); err != nil {
					t.Fatal(err)
				}
				if replacement.WorkDir != cfg.WorkDir || replacement.Listen != cfg.Listen || len(replacement.Allow) != len(cfg.Allow) || replacement.Token != pending.Credential || replacement.Name != "replacement" {
					t.Fatal("replacement lost settings or used stale enrollment fields")
				}
			}
			unchanged, _ := os.ReadFile(config)
			if !bytes.Equal(active, unchanged) {
				t.Fatal("preparation modified the active profile before redemption")
			}
			var saved pendingEnrollment
			if err = store.Read(path, &saved); err != nil || saved.URL != pending.URL || saved.Credential != pending.Credential || saved.IdentityID != pending.IdentityID {
				t.Fatal("replacement was not saved for response recovery", err)
			}
			// Repeating the selected invitation, even with a trailing slash, must
			// reuse its proof rather than fetch metadata or create a credential.
			retry, err := prepareEnrollment(context.Background(), config, pending.URL+"/", false)
			if err != nil || retry.Credential != pending.Credential || retry.IdentityID != pending.IdentityID || retry.Invitation.Name != pending.Invitation.Name || infoRequests.Load() != 1 {
				t.Fatal("retry did not preserve pending redemption", err)
			}
		})
	}
}

func TestInvalidNewInvitationPreservesPendingEnrollment(t *testing.T) {
	for _, failure := range []string{"expired", "gateway", "fleet", "platform", "binary", "name", "reserved-identity", "auto-name", "policy-unavailable"} {
		t.Run(failure, func(t *testing.T) {
			config := filepath.Join(t.TempDir(), "node.json")
			software := buildinfo.Current()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/node/state" {
					http.Error(w, "policy unavailable", http.StatusServiceUnavailable)
					return
				}
				if failure == "expired" {
					http.Error(w, "invitation expired", http.StatusGone)
					return
				}
				invitation := enrollment.Invitation{UserID: "fleet", Name: "new", Gateway: "http://" + r.Host, Asset: update.Asset{OS: software.OS, Arch: software.Arch, SHA256: software.SHA256}}
				switch failure {
				case "gateway":
					invitation.Gateway = "https://other.example"
				case "fleet":
					invitation.UserID = "other-fleet"
				case "platform":
					invitation.Asset.Arch = "unsupported"
				case "binary":
					invitation.Asset.SHA256 = strings.Repeat("0", 64)
				case "name":
					invitation.Name = "invalid name"
				case "reserved-identity":
					invitation.ReplaceID = "another-machine"
				}
				_ = json.NewEncoder(w).Encode(invitation)
			}))
			defer server.Close()
			if err := createNodeConfig(config, "old", server.URL, "active-token", "127.0.0.1:7339", true); err != nil {
				t.Fatal(err)
			}
			previous := pendingEnrollment{URL: server.URL + "/install/old", Credential: "pending-token", Invitation: enrollment.Invitation{Gateway: server.URL, UserID: "fleet"}}
			path := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
			if err := store.Write(path, previous); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			active, _ := os.ReadFile(config)
			if _, err := prepareEnrollment(context.Background(), config, server.URL+"/install/new", failure == "auto-name"); err == nil {
				t.Fatal("invalid invitation accepted")
			}
			after, _ := os.ReadFile(path)
			unchanged, _ := os.ReadFile(config)
			if !bytes.Equal(before, after) || !bytes.Equal(active, unchanged) {
				t.Fatal("failed replacement changed pending recovery or the active profile")
			}
		})
	}
}
