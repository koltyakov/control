package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func TestStandaloneCLIHelper(t *testing.T) {
	if os.Getenv("CONTROL_STANDALONE_TEST_HELPER") == "1" {
		fmt.Println("test-hostname")
	}
}

func TestSavedLoginExecWithoutLocalService(t *testing.T) {
	t.Setenv("CONTROL_HOME", t.TempDir())
	for _, key := range []string{"CONTROL_GATEWAY", "CONTROL_USER_KEY", "CONTROL_SUPERUSER_KEY", "CONTROL_TOKEN", "CONTROL_CONFIG", "CONTROL_API"} {
		t.Setenv(key, "")
	}
	t.Setenv("CONTROL_STANDALONE_TEST_HELPER", "1")
	const super = "standalone-cli-test-superuser-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close() })
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	var account struct{ Token string }
	if err := (client.Admin{URL: s.URL, Key: super}).JSON(ctx, "POST", "/v1/admin/users", map[string]string{"name": "cli-user"}, &account); err != nil {
		t.Fatal(err)
	}
	if err := installation.SaveAdmin(installation.AdminProfile{Gateway: s.URL, Key: account.Token}); err != nil {
		t.Fatal(err)
	}
	w, err := node.New(node.Config{Name: "worker", Gateway: s.URL, Token: account.Token, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	if err := w.Start(ctx); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"exec", "worker", "--", executable, "-test.run=^TestStandaloneCLIHelper$"}); err != nil {
		t.Fatal("login-only execution failed", err)
	}
	var tasks []model.Task
	if err := w.Call(ctx, "", "tasks.list", map[string]any{}, &tasks); err != nil || len(tasks) != 1 || tasks[0].State != "succeeded" {
		t.Fatal("command not executed exactly once", tasks, err)
	}
	var logs struct{ Text string }
	if err := w.Call(ctx, "", "tasks.logs", map[string]any{"id": tasks[0].ID}, &logs); err != nil || !strings.Contains(logs.Text, "test-hostname") {
		t.Fatal("command output missing", logs, err)
	}
	if _, err := os.Stat(installation.ConfigPath()); !os.IsNotExist(err) {
		t.Fatal("standalone command created a host profile", err)
	}
	if err := run(ctx, []string{"exec", "worker", "--detach", "--id", "detached-cli", "--timeout", "2h", "--", executable, "-test.run=^TestStandaloneCLIHelper$"}); err != nil {
		t.Fatal("detached submission", err)
	}
	if err := run(ctx, []string{"task", "wait", "worker", "detached-cli"}); err != nil {
		t.Fatal(err)
	}
	if err := run(ctx, []string{"task", "logs", "worker", "detached-cli", "--follow", "--offset", "0"}); err != nil {
		t.Fatal(err)
	}
	var task model.Task
	if err := w.Call(ctx, "", "tasks.get", map[string]any{"id": "detached-cli"}, &task); err != nil || task.Spec.TimeoutSeconds != 7200 {
		t.Fatal("task timeout lost", task, err)
	}
	if err := run(ctx, []string{"session"}); err != nil {
		t.Fatal("session inspection", err)
	}
	// Explicit API configuration must never silently use gateway credentials.
	api := httptest.NewServer(w.Handler())
	address := api.URL
	api.Close()
	if err := run(ctx, []string{"--api", address, "call", "worker", "node.describe"}); err == nil {
		t.Fatal("explicit API unexpectedly fell back to the gateway")
	}
	t.Setenv("CONTROL_API", address)
	if err := run(ctx, []string{"call", "worker", "node.describe"}); err == nil {
		t.Fatal("CONTROL_API unexpectedly fell back to the gateway")
	}
}
