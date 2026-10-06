package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/secrets"
)

func TestSecretsCLI(t *testing.T) {
	root := t.TempDir()
	cfg := node.Config{DataDir: filepath.Join(root, "state"), WorkDir: filepath.Join(root, "work")}
	if err := os.MkdirAll(cfg.WorkDir, 0700); err != nil {
		t.Fatal(err)
	}
	profile := filepath.Join(root, "node.json")
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(profile, data, 0600); err != nil {
		t.Fatal(err)
	}
	input, err := os.CreateTemp(root, "stdin-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = input.Close() }()
	if _, err := input.WriteString("  private-cli-password  \n"); err != nil {
		t.Fatal(err)
	}
	if _, err := input.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = old }()
	if err := secretsCLI(context.Background(), []string{"set", "app.password", "--stdin"}, profile); err != nil {
		t.Fatal(err)
	}
	s := secrets.Store{Dir: filepath.Join(cfg.DataDir, "secrets"), WorkDir: cfg.WorkDir}
	values, err := s.Snapshot()
	if err != nil || values["app.password"] != "  private-cli-password  " {
		t.Fatalf("value not preserved: %v", err)
	}
	for _, args := range [][]string{{"get", "app.password"}, {"set", "app.password", "secret-as-argument"}, {"list", "extra"}} {
		if err := secretsCLI(context.Background(), args, profile); err == nil || strings.Contains(err.Error(), "private-cli-password") {
			t.Fatal("invalid secret command accepted or value exposed")
		}
	}
	if err := secretsCLI(context.Background(), []string{"list"}, profile); err != nil {
		t.Fatal(err)
	}
	if err := secretsCLI(context.Background(), []string{"delete", "app.password"}, profile); err != nil {
		t.Fatal(err)
	}
	names, err := s.Names()
	if err != nil || len(names) != 0 {
		t.Fatalf("secret not deleted: %v", err)
	}
}
