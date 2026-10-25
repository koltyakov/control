package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
)

func TestConsolePasswordCLI(t *testing.T) {
	const super = "console-cli-superuser-key-at-least-32-characters"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(g.Handler())
	t.Cleanup(func() { s.Close(); _ = g.Close() })
	admin := client.Admin{URL: s.URL, Key: super}
	var user struct{ Token string }
	if err := admin.JSON(context.Background(), "POST", "/v1/admin/users", map[string]string{"name": "alice"}, &user); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	t.Cleanup(func() { os.Stdin = old })
	input := filepath.Join(t.TempDir(), "password")
	run := func(c client.Admin, username, password string) error {
		t.Helper()
		if err := os.WriteFile(input, []byte(password), 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(input)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		os.Stdin = f
		args := []string{"password"}
		if username != "" {
			args = append(args, username)
		}
		return consoleCLI(context.Background(), c, append(args, "--password-stdin"))
	}
	if err := run(admin, "", "operator password with spaces\r\n"); err != nil {
		t.Fatal(err)
	}
	account := client.Admin{URL: s.URL, Key: user.Token}
	if err := run(account, "", "user password with spaces\n"); err != nil {
		t.Fatal(err)
	}
	if err := run(account, "@operator", "attempted escalation"); err == nil {
		t.Fatal("account reset operator password")
	}
	for _, password := range []string{"short", strings.Repeat("x", 1025)} {
		if err := run(admin, "alice", password); err == nil {
			t.Fatal("accepted invalid password length")
		}
	}
	if err := run(admin, "alice", "operator reset alice"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ username, password string }{{"@operator", "operator password with spaces"}, {"alice", "operator reset alice"}} {
		body, err := json.Marshal(map[string]string{"username": tc.username, "password": tc.password})
		if err != nil {
			t.Fatal(err)
		}
		request, err := http.NewRequest("POST", s.URL+"/console/api/login", strings.NewReader(string(body)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Origin", s.URL)
		request.Header.Set("Content-Type", "application/json")
		req, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, req.Body)
		_ = req.Body.Close()
		if req.StatusCode != 200 {
			t.Fatalf("CLI password did not authenticate %s: %d", tc.username, req.StatusCode)
		}
	}
}
