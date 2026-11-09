package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/desktop"
	"github.com/koltyakov/control/internal/model"
)

// desktopCLI forwards the installed tool's whole command surface. Unlike
// exec.run it participates in desktop serialization and sanitizes all text
// before publication. It never resolves a credential into argv or stdin.
func (n *Node) desktopCLI(ctx context.Context, r desktop.Request, home string, e Execution, values map[string]string) (any, error) {
	cmd := command(ctx, Command{Command: n.Config.Desktop.CLI, Args: r.Args}, e.Dir)
	cmd.Env = append(desktopEnvironment(cmd.Env), "AGENT_DESKTOP_HOME="+home)
	if r.Stdin != nil {
		cmd.Stdin = strings.NewReader(*r.Stdin)
	}
	stdout, stderr := &capture{}, &capture{}
	// Never stream unredacted bytes to Execution.Log, including on failure.
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("agent-desktop CLI cancelled; effects may already have occurred; raw diagnostics suppressed: %w", ctx.Err())
	}
	var exited *exec.ExitError
	if stdout.truncated || stderr.truncated || (err != nil && !errors.As(err, &exited)) {
		return nil, errors.New("agent-desktop CLI failed, was cancelled, or exceeded output limits; effects may already have occurred; raw diagnostics suppressed")
	}
	if !utf8.Valid(stdout.b.Bytes()) || !utf8.Valid(stderr.b.Bytes()) {
		return nil, errors.New("agent-desktop CLI returned non-text output; use declared task output artifacts for binary data; effects may already have occurred")
	}
	exitCode := cmd.ProcessState.ExitCode()
	var envelope map[string]json.RawMessage
	var ok bool
	if json.Unmarshal(stdout.b.Bytes(), &envelope) != nil || envelope == nil ||
		json.Unmarshal(envelope["ok"], &ok) != nil || string(envelope["ok"]) == "null" {
		// Help and other textual CLI output remain usable, but are redacted
		// just like JSON. Exit status, never a successful transport, is outcome.
		key := "data"
		if err != nil {
			key = "error"
		}
		envelope = map[string]json.RawMessage{
			"ok": model.JSON(err == nil),
			key:  model.JSON(map[string]any{"stdout": stdout.b.String()}),
		}
	} else if err != nil && ok {
		// A contradictory successful envelope cannot hide process failure.
		envelope["ok"] = model.JSON(false)
		envelope["error"] = model.JSON(map[string]any{"code": "CLI_EXIT_FAILED"})
	}
	envelope["process"] = model.JSON(map[string]any{"exitCode": exitCode, "stderr": stderr.b.String()})
	value, responseErr := desktopResponse(model.JSON(envelope), values, home, false, true)
	if responseErr == nil && desktopBatchFailed(envelope) {
		return value, errors.New("agent-desktop batch contains failed or unstarted entries; inspect sanitized partial results; never replay automatically")
	}
	return value, responseErr
}

// Upstream batch can exit zero with ok:true despite failed entries. Preserve
// that original envelope, but do not let dependent Control work assume success.
func desktopBatchFailed(envelope map[string]json.RawMessage) bool {
	var command string
	if json.Unmarshal(envelope["command"], &command) != nil || command != "batch" {
		return false
	}
	var batch struct {
		Results []struct {
			OK *bool `json:"ok"`
		} `json:"results"`
		NotStarted int             `json:"not_started_entries"`
		Stopped    json.RawMessage `json:"stopped"`
	}
	if json.Unmarshal(envelope["data"], &batch) != nil || batch.Results == nil || batch.NotStarted != 0 ||
		(len(batch.Stopped) > 0 && string(batch.Stopped) != "null") {
		return true
	}
	for _, result := range batch.Results {
		if result.OK == nil || !*result.OK {
			return true
		}
	}
	return false
}
