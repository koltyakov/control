package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/secrets"
)

func (n *Node) execRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	var input struct {
		Command
		Stdin       *string           `json:"stdin"`
		StdinSecret *string           `json:"stdinSecret"`
		EnvSecrets  map[string]string `json:"envSecrets"`
	}
	if err := json.Unmarshal(args, &input); err != nil {
		return nil, err
	}
	if input.Stdin != nil && input.StdinSecret != nil {
		return nil, errors.New("stdin and stdinSecret are mutually exclusive")
	}
	if len(input.EnvSecrets) > 32 {
		return nil, errors.New("at most 32 secret environment variables are allowed")
	}
	seen := map[string]bool{}
	for name := range input.EnvSecrets {
		if !validSecretEnvName(name) {
			return nil, errors.New("invalid secret environment variable name")
		}
		key := strings.ToUpper(name)
		if seen[key] {
			return nil, errors.New("secret environment variable names must be unique ignoring case")
		}
		seen[key] = true
		for literal := range input.Env {
			if strings.EqualFold(name, literal) {
				return nil, errors.New("environment variable cannot have both a literal value and a secret reference")
			}
		}
	}
	stdin := ""
	if input.Stdin != nil {
		stdin = *input.Stdin
	}
	if input.StdinSecret == nil && len(input.EnvSecrets) == 0 {
		return runCommand(ctx, input.Command, e, stdin)
	}
	values, err := n.secretValues()
	if err != nil {
		return nil, err
	}
	redactor := secrets.NewRedactor(values)
	if input.Env == nil {
		input.Env = map[string]string{}
	}
	for name, ref := range input.EnvSecrets {
		value, err := resolveSecret(values, ref)
		if err != nil {
			return nil, err
		}
		input.Env[name] = value
	}
	if input.StdinSecret != nil {
		stdin, err = resolveSecret(values, *input.StdinSecret)
		if err != nil {
			return nil, err
		}
	}
	// Never stream raw chunks: credentials may cross write/capture boundaries.
	// The original JSON and persisted task arguments retain only references.
	private := e
	private.Log = io.Discard
	result, runErr := runCommand(ctx, input.Command, private, stdin)
	if result.Truncated || ctx.Err() != nil || errors.Is(runErr, exec.ErrWaitDelay) || !utf8.ValidString(result.Stdout) || !utf8.ValidString(result.Stderr) {
		// Truncated output keeps a head and tail, which are never masked or
		// published here: suppression remains all-or-nothing.
		result.Stdout, result.Stderr = "", ""
		result.StdoutOmitted, result.StderrOmitted = 0, 0
		return result, errors.New("secret-bearing command output is incomplete or non-UTF-8; output suppressed")
	}
	result.Stdout = redactCommandText(redactor, result.Stdout)
	result.Stderr = redactCommandText(redactor, result.Stderr)
	if runErr != nil {
		if clean := redactor.Text(runErr.Error()); clean != runErr.Error() {
			runErr = errors.New(clean)
		}
	}
	if e.Log != nil {
		for _, text := range []string{result.Stdout, result.Stderr} {
			if _, err := io.WriteString(e.Log, text); err != nil {
				return result, errors.Join(runErr, errors.New("cannot write sanitized command logs"))
			}
		}
	}
	return result, runErr
}

func validSecretEnvName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i, ch := range []byte(name) {
		if ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || i > 0 && ch >= '0' && ch <= '9' {
			continue
		}
		return false
	}
	return true
}

func redactCommandText(redactor secrets.Redactor, text string) string {
	if json.Valid([]byte(text)) {
		if clean, err := redactor.JSON([]byte(text)); err == nil {
			// JSON numbers can also echo numeric credentials. Stdout is a text
			// field, so masking those tokens need not preserve upstream JSON.
			return redactor.Text(string(clean))
		}
	}
	return redactor.Text(text)
}
