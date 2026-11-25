package node

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestCommandUsageProcess(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_USAGE")
	if mode == "" {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	switch mode {
	case "provider":
		fmt.Print(`{"ok":true}`)
	case "secret-binary":
		// Non-UTF-8 output on the secret path is suppressed entirely.
		_, _ = os.Stdout.Write([]byte{0xff})
		fmt.Fprint(os.Stderr, os.Getenv("CONTROL_TEST_PASSWORD"))
	}
	os.Exit(0)
}

func usageCommand(t *testing.T, mode string) Command {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Command{Command: path, Args: []string{"-test.run=^TestCommandUsageProcess$"}, Env: map[string]string{"CONTROL_TEST_USAGE": mode}}
}

func checkUsage(t *testing.T, usage *model.CommandUsage) {
	t.Helper()
	if usage == nil || usage.WallMs == nil || usage.UserCPUMs == nil || usage.SystemCPUMs == nil {
		t.Fatalf("command usage missing: %+v", usage)
	}
	memory, absent := usage.MaxRSSBytes, usage.PeakJobMemoryBytes
	if runtime.GOOS == "windows" {
		memory, absent = absent, memory
	}
	if memory == nil || *memory <= 0 || absent != nil {
		t.Fatalf("memory reading: %s", model.JSON(usage))
	}
}

func TestExecRunReportsUsage(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	value, err := n.execRun(t.Context(), model.JSON(usageCommand(t, "provider")), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	result := value.(commandResult)
	checkUsage(t, result.Usage)
	if encoded := string(model.JSON(result)); !strings.Contains(encoded, `"usage":{"wallMs":`) {
		t.Fatal("usage absent from the command result", encoded)
	}
}

func TestSecretCommandSuppressesOutputButReportsUsage(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	setTestSecret(t, n, "usage.password", "usage-private-token")
	cmd := usageCommand(t, "secret-binary")
	args := map[string]any{"command": cmd.Command, "args": cmd.Args, "env": cmd.Env, "envSecrets": map[string]string{"CONTROL_TEST_PASSWORD": "usage.password"}}
	value, err := n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
	result := value.(commandResult)
	if err == nil || result.Stdout != "" || result.Stderr != "" || strings.Contains(string(model.JSON(result)), "usage-private-token") {
		t.Fatal("secret-bearing output was published", err, result)
	}
	checkUsage(t, result.Usage)
}

func TestTaskRecordCarriesCommandUsage(t *testing.T) {
	t.Parallel()
	n := localNode(t, func(cfg *Config) {
		cfg.Providers = map[string]Command{"custom.usage": usageCommand(t, "provider")}
	})
	for _, spec := range []model.TaskSpec{
		{ID: "provider-usage", Capability: "custom.usage", Args: model.JSON(map[string]any{})},
		{ID: "exec-usage", Capability: "exec.run", Args: model.JSON(usageCommand(t, "provider"))},
	} {
		task, err := n.startTaskContext(t.Context(), "owner", spec)
		if err != nil {
			t.Fatal(err)
		}
		if task, err = n.waitTask(t.Context(), "owner", task.ID, time.Minute); err != nil || task.State != "succeeded" {
			t.Fatal(spec.ID, task.State, task.Error, err)
		}
		checkUsage(t, task.Usage)
		if spec.Capability == "custom.usage" && string(task.Result) != `{"ok":true}` {
			t.Fatal("provider result changed", string(task.Result))
		}
		// The durable record keeps usage; summaries stay compact.
		data, err := os.ReadFile(n.taskPath(task.ID))
		var stored model.Task
		if err != nil || json.Unmarshal(data, &stored) != nil || stored.Usage == nil || *stored.Usage.WallMs != *task.Usage.WallMs {
			t.Fatal("persisted record lost usage", err)
		}
		if strings.Contains(string(model.JSON(model.SummarizeTask(task))), `"usage":`) {
			t.Fatal("task summary includes usage")
		}
	}
	// Non-command tasks carry no usage.
	task, err := n.startTaskContext(t.Context(), "owner", model.TaskSpec{ID: "list", Capability: "files.list", Args: model.JSON(map[string]any{"path": "."})})
	if err != nil {
		t.Fatal(err)
	}
	if task, err = n.waitTask(t.Context(), "owner", task.ID, time.Minute); err != nil || task.Usage != nil {
		t.Fatal("non-command task reported usage", task.Usage, err)
	}
}
