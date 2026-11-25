// Package shellpty starts a fixed interactive shell attached to a native
// pseudo-terminal: a Unix PTY on Linux and macOS, and ConPTY on Windows.
//
// Callers choose a shell by its exact name from Names; no paths, arguments,
// or command strings are accepted. The executable is discovered on the local
// machine and started with fixed arguments.
//
// A Terminal reads the combined terminal output and writes keyboard input.
// Windows cleans up its job object. Linux uses pidfds where available to
// signal other processes in the shell's session; older Linux and macOS rely
// on terminal hangup. Wait reports the shell's exit status independently of
// EOF. Cancellation or Close terminates the shell and releases the terminal.
// Callers must always call Close.
package shellpty

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// MaxDimension is the largest accepted column or row count. Windows
	// represents terminal sizes as signed 16-bit coordinates.
	MaxDimension = 32767
	// MaxTermLength is the longest accepted TERM value.
	MaxTermLength = 64
	// DefaultTerm is used on Unix when the caller supplies no TERM value.
	DefaultTerm = "xterm-256color"
)

var (
	// ErrUnsupportedShell reports a name outside Names for this platform.
	ErrUnsupportedShell = errors.New("unsupported shell")
	// ErrShellNotFound reports a supported shell that is not installed.
	ErrShellNotFound = errors.New("shell is not installed")
	// ErrUnsupportedPlatform reports an operating system without a backend.
	ErrUnsupportedPlatform = errors.New("interactive shells are not supported on this platform")
	// ErrClosed is returned by Wait when Close terminated the shell.
	ErrClosed = errors.New("terminal closed")
)

// closeTimeout bounds how long Close waits for the shell to be reaped and the
// pseudo-terminal to be released. Cleanup continues in the background if a
// process ignores termination, such as one blocked in the kernel.
const closeTimeout = 5 * time.Second

// Names lists the shell names accepted on this platform, in preference order.
// Not every name is necessarily installed; see Available.
func Names() []string { return slices.Clone(shellNames) }

// Available lists the accepted shell names whose executable is installed.
func Available() []string {
	var names []string
	for _, name := range shellNames {
		if _, err := resolveShell(name); err == nil {
			names = append(names, name)
		}
	}
	return names
}

// Default is the preferred shell name for this platform. It may still be
// missing on an unusual installation.
func Default() string {
	if len(shellNames) == 0 {
		return ""
	}
	return shellNames[0]
}

// shell is a resolved executable and its fixed argument vector. args[0] is
// the program name the shell sees.
type shell struct {
	path string
	args []string
}

// ValidateSize reports whether cols and rows are within 1..MaxDimension.
func ValidateSize(cols, rows uint16) error {
	if cols == 0 || rows == 0 || cols > MaxDimension || rows > MaxDimension {
		return fmt.Errorf("terminal size %dx%d must be within 1..%d", cols, rows, MaxDimension)
	}
	return nil
}

// ValidateTerm reports whether term is empty or at most MaxTermLength ASCII
// letters, digits, underscores, dashes, dots, or plus signs.
func ValidateTerm(term string) error {
	if len(term) > MaxTermLength {
		return fmt.Errorf("TERM is longer than %d characters", MaxTermLength)
	}
	for i := 0; i < len(term); i++ {
		c := term[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == '+':
		default:
			return errors.New("TERM may contain only ASCII letters, digits, '_', '-', '.', and '+'")
		}
	}
	return nil
}

// validateDir returns the starting directory. Empty means the current
// process directory; otherwise the path must be an absolute directory.
func validateDir(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("starting directory %q is not absolute", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("starting directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("starting directory %q is not a directory", dir)
	}
	return filepath.Clean(dir), nil
}

func validateStart(name string, cols, rows uint16, term, dir string) (shell, string, error) {
	if !slices.Contains(shellNames, name) {
		return shell{}, "", fmt.Errorf("%w %q; supported shells: %v", ErrUnsupportedShell, name, shellNames)
	}
	if err := ValidateSize(cols, rows); err != nil {
		return shell{}, "", err
	}
	if err := ValidateTerm(term); err != nil {
		return shell{}, "", err
	}
	cleanDir, err := validateDir(dir)
	if err != nil {
		return shell{}, "", err
	}
	resolved, err := resolveShell(name)
	if err != nil {
		return shell{}, "", err
	}
	return resolved, cleanDir, nil
}

// withTerm returns env without inherited TERM entries and, when term is not
// empty, with TERM=term appended. Keys compare case-insensitively when fold
// is set, as on Windows.
func withTerm(env []string, term string, fold bool) []string {
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		key := entry
		if i := indexEquals(entry); i >= 0 {
			key = entry[:i]
		}
		if key == "TERM" || (fold && strings.EqualFold(key, "TERM")) {
			continue
		}
		out = append(out, entry)
	}
	if term != "" {
		out = append(out, "TERM="+term)
	}
	return out
}

// indexEquals finds the key separator, skipping a leading '=' used by
// Windows per-drive directory entries such as "=C:=C:\".
func indexEquals(entry string) int {
	for i := 1; i < len(entry); i++ {
		if entry[i] == '=' {
			return i
		}
	}
	return -1
}
