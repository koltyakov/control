package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// Only synthetic credentials are used. No installed SDKs, shells or fleet.
func TestExecSecretProcess(t *testing.T) {
	mode := os.Getenv("CONTROL_TEST_EXEC_SECRET")
	if mode == "" {
		return
	}
	if path := os.Getenv("CONTROL_TEST_EXEC_MARKER"); path != "" {
		if os.WriteFile(path, []byte("started"), 0600) != nil {
			os.Exit(3)
		}
	}
	value := os.Getenv("CONTROL_TEST_PASSWORD")
	stdin, _ := io.ReadAll(os.Stdin)
	switch mode {
	case "echo", "fail":
		// Exercise independent chunks and streams, and common encodings.
		for _, ch := range []byte(value) {
			_, _ = os.Stdout.Write([]byte{ch})
		}
		fmt.Fprintln(os.Stdout, "|"+string(stdin)+"|"+os.Getenv("CONTROL_TEST_LITERAL"))
		fmt.Fprint(os.Stderr, base64.StdEncoding.EncodeToString([]byte(value)))
		if mode == "fail" {
			os.Exit(7)
		}
	case "json":
		fmt.Fprint(os.Stdout, `{"password":"\u0070rivate-token"}`)
	case "overflow":
		fmt.Print(strings.Repeat("x", (2<<20)-3) + value)
		fmt.Fprint(os.Stderr, value)
	case "binary":
		_, _ = os.Stdout.Write([]byte{0xff})
		fmt.Fprint(os.Stderr, value)
	case "sleep":
		fmt.Print(value)
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func execSecretArgs(t *testing.T, mode string) map[string]any {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return map[string]any{
		"command": path,
		"args":    []string{"-test.run=^TestExecSecretProcess$"},
		"env": map[string]string{
			"CONTROL_TEST_EXEC_SECRET": mode,
			"CONTROL_TEST_LITERAL":     "ordinary",
		},
		"envSecrets": map[string]string{"CONTROL_TEST_PASSWORD": "domain.admin.password"},
		"stdinSecret": "stdin.password",
	}
}

func TestExecSecretInjection(t *testing.T) {
	t.Setenv("CONTROL_TEST_PASSWORD", "inherited-must-be-overridden")
	n := localNode(t, nil)
	setTestSecret(t, n, "domain.admin.password", "private-token")
	setTestSecret(t, n, "stdin.password", "exact stdin\n")
	for _, mode := range []string{"echo", "fail", "json", "overflow", "binary"} {
		t.Run(mode, func(t *testing.T) {
			args := model.JSON(execSecretArgs(t, mode))
			before := bytes.Clone(args)
			var logs bytes.Buffer
			value, err := n.execRun(t.Context(), args, Execution{Dir: n.Config.WorkDir, Log: &logs})
			result := value.(commandResult)
			if (mode == "echo" || mode == "json") != (err == nil) {
				t.Fatalf("unexpected outcome: %v", err)
			}
			if !bytes.Equal(before, args) {
				t.Fatal("mutated caller arguments")
			}
			for _, output := range []string{string(model.JSON(result)), logs.String(), fmt.Sprint(err)} {
				for _, secret := range []string{"private-token", "exact stdin", "inherited-must-be-overridden", base64.StdEncoding.EncodeToString([]byte("private-token"))} {
					if strings.Contains(output, secret) {
						t.Fatal("command output leaked a credential")
					}
				}
			}
			switch mode {
			case "echo", "fail":
				if result.Stdout != "[REDACTED]|[REDACTED]|ordinary\n" || result.Stderr != "[REDACTED]" || logs.String() != result.Stdout+result.Stderr {
					t.Fatal("injection or sanitized logs incorrect")
				}
				if mode == "fail" && result.ExitCode != 7 {
					t.Fatal("lost command exit status")
				}
			case "json":
				if result.Stdout != `{"password":"[REDACTED]"}` {
					t.Fatal("JSON-escaped credential was not masked")
				}
			case "overflow", "binary":
				if result.Stdout != "" || result.Stderr != "" || logs.Len() != 0 {
					t.Fatal("published unsafe partial/binary output")
				}
			}
		})
	}
	if os.Getenv("CONTROL_TEST_PASSWORD") != "inherited-must-be-overridden" {
		t.Fatal("modified node environment")
	}
	// Exact stdin, without an appended newline, and no environment injection.
	args := execSecretArgs(t, "echo")
	delete(args, "envSecrets")
	args["env"].(map[string]string)["CONTROL_TEST_PASSWORD"] = ""
	setTestSecret(t, n, "stdin.password", "no-newline")
	value, err := n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
	if err != nil || value.(commandResult).Stdout != "|[REDACTED]|ordinary\n" {
		t.Fatal("stdinSecret did not supply exact bytes", err)
	}
	// Ordinary stdin can accompany secret environment injection.
	args = execSecretArgs(t, "echo")
	delete(args, "stdinSecret")
	args["stdin"] = "public"
	value, err = n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: io.Discard})
	if err != nil || value.(commandResult).Stdout != "[REDACTED]|public|ordinary\n" {
		t.Fatal("ordinary stdin with envSecrets failed", err)
	}
}

func TestExecSecretValidationBeforeLaunch(t *testing.T) {
	n := localNode(t, nil)
	setTestSecret(t, n, "domain.admin.password", "private-token")
	setTestSecret(t, n, "stdin.password", "stdin-token")
	for _, name := range []string{"stdin conflict", "missing stdin", "empty stdin ref", "missing env", "invalid ref", "invalid env", "case conflict", "duplicate case", "too many"} {
		t.Run(name, func(t *testing.T) {
			args := execSecretArgs(t, "echo")
			marker := filepath.Join(t.TempDir(), "started")
			args["env"].(map[string]string)["CONTROL_TEST_EXEC_MARKER"] = marker
			switch name {
			case "stdin conflict":
				args["stdin"] = ""
			case "missing stdin":
				args["stdinSecret"] = "missing"
			case "empty stdin ref":
				args["stdinSecret"] = ""
			case "missing env":
				args["envSecrets"] = map[string]string{"PASSWORD": "missing"}
			case "invalid ref":
				args["envSecrets"] = map[string]string{"PASSWORD": "../values"}
			case "invalid env":
				args["envSecrets"] = map[string]string{"A=B": "domain.admin.password"}
			case "case conflict":
				args["env"].(map[string]string)["control_test_password"] = "literal"
			case "duplicate case":
				args["envSecrets"] = map[string]string{"PASSWORD": "domain.admin.password", "password": "stdin.password"}
			case "too many":
				refs := map[string]string{}
				for i := range 33 {
					refs[fmt.Sprintf("PASSWORD_%d", i)] = "domain.admin.password"
				}
				args["envSecrets"] = refs
			}
			if _, err := n.execRun(t.Context(), model.JSON(args), Execution{Dir: n.Config.WorkDir, Log: io.Discard}); err == nil {
				t.Fatal("accepted invalid references")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("launched command before validating references")
			}
		})
	}
	for _, name := range []string{"", "1A", "A-B", "A B", "A\x00", "é", strings.Repeat("A", 129)} {
		if validSecretEnvName(name) {
			t.Fatal("accepted invalid environment variable name")
		}
	}
	for _, name := range []string{"A", "_", "AWS_SECRET_ACCESS_KEY", "AZURE_CLIENT_SECRET", "A1", strings.Repeat("A", 128)} {
		if !validSecretEnvName(name) {
			t.Fatal("rejected portable environment variable name")
		}
	}
}

func TestExecSecretLaunchErrorAndCancellation(t *testing.T) {
	n := localNode(t, nil)
	setTestSecret(t, n, "domain.admin.password", "private-token")
	setTestSecret(t, n, "stdin.password", "stdin-token")
	args := execSecretArgs(t, "echo")
	args["command"] = filepath.Join(t.TempDir(), "private-token")
	_, err := n.execRun(t.Context(), model.JSON(args), Execution{Log: io.Discard})
	if err == nil || strings.Contains(err.Error(), "private-token") || !strings.Contains(err.Error(), "[REDACTED]") {
		t.Fatal("launch error not sanitized", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	var logs bytes.Buffer
	start := time.Now()
	value, err := n.execRun(ctx, model.JSON(execSecretArgs(t, "sleep")), Execution{Dir: n.Config.WorkDir, Log: &logs})
	if err == nil || time.Since(start) > 5*time.Second || strings.Contains(logs.String()+string(model.JSON(value))+err.Error(), "private-token") {
		t.Fatal("cancellation failed or leaked credentials")
	}
}

func TestExecSecretSchema(t *testing.T) {
	n := localNode(t, nil)
	for _, capability := range n.Capabilities() {
		if capability.Name != "exec.run" {
			continue
		}
		var input struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(capability.InputSchema, &input); err != nil || input.Properties["envSecrets"] == nil || input.Properties["stdinSecret"] == nil {
			t.Fatal("exec.run does not advertise secret injection")
		}
		return
	}
	t.Fatal("exec.run is not registered")
}

func TestExecSecretPeerTaskRetention(t *testing.T) {
	for _, relay := range []bool{true, false} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			source, worker, _ := cluster(t, relay, nil)
			setTestSecret(t, worker, "domain.admin.password", "private-token")
			setTestSecret(t, worker, "stdin.password", "stdin-token")
			spec := model.TaskSpec{ID: "exec-secret-task", Capability: "exec.run", Args: model.JSON(execSecretArgs(t, "echo"))}
			var accepted model.Task
			call(t, source, "worker", "tasks.start", spec, &accepted)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			finished, err := testWaitTask(t, source, ctx, "worker", accepted.ID)
			if err != nil || finished.State != "succeeded" || !bytes.Equal(finished.Spec.Args, spec.Args) {
				t.Fatal("secret task failed or changed references", err, finished.Error)
			}
			for _, path := range []string{worker.taskPath(spec.ID), worker.logPath(spec.ID)} {
				data, err := os.ReadFile(path)
				if err != nil || bytes.Contains(data, []byte("private-token")) || bytes.Contains(data, []byte("stdin-token")) || !bytes.Contains(data, []byte("[REDACTED]")) {
					t.Fatal("task persistence did not retain sanitized output", err)
				}
			}
			// Rotation must not replay an accepted ID.
			setTestSecret(t, worker, "domain.admin.password", "rotated-token")
			call(t, source, "worker", "tasks.start", spec, &accepted)
			if accepted.Updated != finished.Updated {
				t.Fatal("secret rotation replayed accepted task")
			}
		})
	}
}
