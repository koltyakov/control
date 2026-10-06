package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

const updateAuthorizationTestKey = "update-authorization-superuser-key-1234567890"

func updateAuthorizationEnvironment(t *testing.T) (client.Admin, installation.AdminProfile) {
	t.Helper()
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG"} {
		t.Setenv(name, "")
	}
	g, err := gateway.New(t.TempDir(), "common-update-test-key", gateway.Options{SuperuserKey: updateAuthorizationTestKey})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	server := httptest.NewServer(g.Handler())
	t.Cleanup(server.Close)
	var account struct{ Token string }
	operator := client.Admin{URL: server.URL, Key: updateAuthorizationTestKey}
	if err := operator.JSON(t.Context(), "POST", "/v1/admin/users", map[string]string{"name": "update-user"}, &account); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"login", "--gateway", server.URL, "--api-key", account.Token}); err != nil {
		t.Fatal(err)
	}
	return client.Admin{URL: server.URL, Key: account.Token}, installation.AdminProfile{Gateway: server.URL, Key: account.Token}
}

func TestLocalUpdateNeedsNoGatewayOrProfile(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_RELEASE_REPO", "")
	for _, command := range []string{"update", "upgrade"} {
		for _, help := range []string{"--help", "help"} {
			if err := run(t.Context(), []string{"--config", t.TempDir(), command, help}); err != nil {
				t.Fatalf("%s %s: %v", command, help, err)
			}
		}
		// A missing release repository must be reported without gateway auth.
		err := run(t.Context(), []string{"--config", t.TempDir(), command})
		if err == nil || !strings.Contains(err.Error(), "release repository") {
			t.Fatalf("%s: %v", command, err)
		}
		if err := run(t.Context(), []string{command, "unexpected"}); err == nil {
			t.Fatal("accepted unexpected positional argument")
		}
	}
	if !strings.Contains(usage, "update [--version TAG]") {
		t.Fatal("ordinary help omits local update")
	}
}

func TestManagedUpdateUsesSuperuserKeyAndExplicitTokenOverride(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	t.Setenv("CONTROL_USER_KEY", "account-key")
	t.Setenv("CONTROL_SUPERUSER_KEY", "superuser-key")
	t.Setenv("CONTROL_TOKEN", "common-key")
	t.Setenv("CONTROL_CONFIG", "")
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if r.URL.Path == "/v1/auth" {
			switch key {
			case "superuser-key":
				_, _ = io.WriteString(w, `{"role":"superuser"}`)
			case "account-key":
				_, _ = io.WriteString(w, `{"role":"user"}`)
			case "common-key":
				_, _ = io.WriteString(w, `{"role":"common"}`)
			default:
				http.Error(w, "unauthorized", http.StatusUnauthorized)
			}
			return
		}
		if key != "superuser-key" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		requests <- r.Method + " " + r.URL.Path
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	t.Setenv("CONTROL_GATEWAY", server.URL)
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: server.URL, Key: "account-key"}); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	binary := []byte("local development build")
	asset := update.Asset{OS: "linux", Arch: "amd64", File: update.AssetName("linux", "amd64"), Size: int64(len(binary)), SHA256: fmt.Sprintf("%x", sha256.Sum256(binary))}
	if err := os.WriteFile(filepath.Join(dir, asset.File), binary, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Write(filepath.Join(dir, "control-manifest.json"), update.Manifest{Version: "dev-local", CreatedAt: time.Now().UTC(), Assets: []update.Asset{asset}}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"update", "push", dir}, {"update", "status"}, {"update", "check"}} {
		if err := run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	want := []string{"PUT /v1/admin/updates/blobs/" + asset.SHA256, "POST /v1/admin/updates", "GET /v1/admin/updates", "POST /v1/admin/updates/check"}
	if len(requests) != len(want) {
		t.Fatalf("got %d requests, want %d", len(requests), len(want))
	}
	for _, expected := range want {
		if got := <-requests; got != expected {
			t.Fatalf("request = %q, want %q", got, expected)
		}
	}
	for _, tc := range []struct{ key, want string }{
		{"account-key", "require the gateway superuser key"},
		{"common-key", "require the gateway superuser key"},
		{"wrong-key", "401 Unauthorized"},
		{"", "gateway credential is not configured"},
	} {
		for _, args := range [][]string{{"update", "push", dir}, {"update", "status"}, {"update", "check"}} {
			err := run(t.Context(), append([]string{"--token", tc.key}, args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("%v with explicit credential: got %v, want %q", args, err, tc.want)
			}
		}
	}
	if len(requests) != 0 {
		t.Fatal("rejected commands reached update endpoints")
	}
	server.Close()
	err := run(t.Context(), []string{"update", "status"})
	if err == nil || !strings.Contains(err.Error(), "authenticate with gateway "+server.URL) {
		t.Fatalf("lost gateway connection error: %v", err)
	}
}

func TestManagedUpdateReportsConfigurationErrors(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, name := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG"} {
		t.Setenv(name, "")
	}
	if err := run(t.Context(), []string{"update", "status"}); err == nil || !strings.Contains(err.Error(), "gateway URL is not configured") {
		t.Fatalf("missing gateway: %v", err)
	}
	t.Setenv("CONTROL_GATEWAY", "https://pool.example")
	if err := run(t.Context(), []string{"update", "status"}); err == nil || !strings.Contains(err.Error(), "gateway credential is not configured") {
		t.Fatalf("missing credential: %v", err)
	}
	profile := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(profile, []byte("invalid JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"--config", profile, "update", "status"}); err == nil || strings.Contains(err.Error(), "gateway credential") || strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("lost profile error: %v", err)
	}
}

func TestUpdateAuthorizationSavedSeparatelyAndReused(t *testing.T) {
	c, login := updateAuthorizationEnvironment(t)
	prompts := 0
	if err := authorizeUpdates(t.Context(), c, func() (string, error) {
		prompts++
		return " " + updateAuthorizationTestKey + "\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	saved, err := installation.ReadUpdateAdmin()
	if err != nil || saved != (installation.AdminProfile{Gateway: c.URL, Key: updateAuthorizationTestKey}) {
		t.Fatalf("update authorization not saved: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(installation.Home(), "update-admin.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("update credential is not private: %v", err)
		}
	}
	remote, err := updateAdminClient(installation.ConfigPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := authorizeUpdates(t.Context(), remote, func() (string, error) {
		prompts++
		return "", errors.New("unexpected second prompt")
	}); err != nil || prompts != 1 {
		t.Fatalf("authorization was not reused: %v, prompts %d", err, prompts)
	}
	for _, args := range [][]string{{"update", "authorize"}, {"update", "status"}, {"machines"}, {"dashboard", "--json"}} {
		if err := run(t.Context(), args); err != nil {
			t.Fatalf("saved credentials failed for %v: %v", args, err)
		}
	}
	after, err := installation.ReadAdmin()
	if err != nil || after != login {
		t.Fatal("update authorization replaced dashboard login", err)
	}
	if fleet := adminClient(installation.ConfigPath()); fleet.Key != c.Key || strings.Contains(commandHelp(t.Context(), fleet), "update push") {
		t.Fatal("saved update key elevated ordinary fleet commands or help")
	}
	if err := c.JSON(t.Context(), "GET", "/v1/admin/updates", nil, nil); err == nil {
		t.Fatal("ordinary account gained gateway update authority")
	}
}

func TestUpdateAuthorizationRejectsKeysWithoutChangingProfiles(t *testing.T) {
	c, login := updateAuthorizationEnvironment(t)
	previous := installation.AdminProfile{Gateway: c.URL, Key: updateAuthorizationTestKey}
	if err := installation.SaveUpdateAdmin(previous); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ key, want string }{
		{"wrong-key", "401 Unauthorized"},
		{c.Key, "not a gateway superuser key"},
		{"common-update-test-key", "not a gateway superuser key"},
		{" \n", "superuser key is required"},
		{strings.Repeat("k", 4097), "exceeds 4096 bytes"},
	} {
		err := authorizeUpdates(t.Context(), c, func() (string, error) { return tc.key, nil })
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("got %v, want %q", err, tc.want)
		}
		if saved, err := installation.ReadUpdateAdmin(); err != nil || saved != previous {
			t.Fatal("invalid key changed update authorization", err)
		}
		if saved, err := installation.ReadAdmin(); err != nil || saved != login {
			t.Fatal("invalid key changed dashboard login", err)
		}
	}
	if err := authorizeUpdates(t.Context(), c, nil); !errors.Is(err, errUpdatePermission) {
		t.Fatalf("noninteractive authorization did not fail clearly: %v", err)
	}
	if err := authorizeUpdates(t.Context(), c, func() (string, error) { return "", context.Canceled }); !errors.Is(err, context.Canceled) {
		t.Fatalf("prompt cancellation was lost: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := authorizeUpdates(ctx, c, func() (string, error) {
		t.Fatal("prompted after authentication was cancelled")
		return updateAuthorizationTestKey, nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("authentication cancellation was lost: %v", err)
	}
}

func TestSavedUpdateAuthorizationIsGatewayScopedAndRespectsOverrides(t *testing.T) {
	c, _ := updateAuthorizationEnvironment(t)
	if err := installation.SaveUpdateAdmin(installation.AdminProfile{Gateway: c.URL + "/", Key: updateAuthorizationTestKey}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONTROL_USER_KEY", c.Key)
	t.Setenv("CONTROL_TOKEN", "common-update-test-key")
	if err := run(t.Context(), []string{"update", "status"}); err != nil {
		t.Fatal("account/node environment masked saved update authorization", err)
	}
	for _, key := range []string{c.Key, "common-update-test-key", "wrong-key", ""} {
		if err := run(t.Context(), []string{"--token", key, "update", "status"}); err == nil {
			t.Fatal("explicit credential was silently elevated")
		}
	}
	t.Setenv("CONTROL_SUPERUSER_KEY", "wrong-key")
	if err := run(t.Context(), []string{"update", "status"}); err == nil || !strings.Contains(err.Error(), "401 Unauthorized") {
		t.Fatalf("explicit superuser environment was ignored: %v", err)
	}
	t.Setenv("CONTROL_SUPERUSER_KEY", "")
	t.Setenv("CONTROL_GATEWAY", "https://other.example")
	remote, err := updateAdminClient(installation.ConfigPath(), nil)
	if err != nil || remote.Key == updateAuthorizationTestKey {
		t.Fatal("saved update credential selected for another gateway", err)
	}
}

func TestUpdateAuthorizeCLIReadsKeyFromStdin(t *testing.T) {
	_, login := updateAuthorizationEnvironment(t)
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(updateAuthorizationTestKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	previous := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = previous }()
	if err := run(t.Context(), []string{"update", "authorize", "--check"}); !errors.Is(err, errUpdatePermission) {
		t.Fatalf("check-only authorization did not report missing permission: %v", err)
	}
	if pos, err := input.Seek(0, io.SeekCurrent); err != nil || pos != 0 {
		t.Fatal("check-only authorization read a key from stdin", err)
	}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved.Key != "" {
		t.Fatal("check-only authorization saved credentials", err)
	}
	if err := run(t.Context(), []string{"update", "authorize", "--key-stdin"}); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"update", "authorize"}); err != nil {
		t.Fatal("subsequent CLI invocation needed authorization again", err)
	}
	if err := run(t.Context(), []string{"update", "authorize", "--check"}); err != nil {
		t.Fatal("check-only authorization did not reuse saved credentials", err)
	}
	if saved, err := installation.ReadUpdateAdmin(); err != nil || saved.Key != updateAuthorizationTestKey {
		t.Fatal("stdin authorization was not saved", err)
	}
	if saved, err := installation.ReadAdmin(); err != nil || saved != login {
		t.Fatal("stdin authorization changed normal login", err)
	}
	if err := run(t.Context(), []string{"--token", login.Key, "update", "authorize", "--key-stdin"}); err == nil {
		t.Fatal("stdin silently replaced an explicit token")
	}
}
