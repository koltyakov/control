package model

import (
	"errors"
	"strings"
)

const ShellOpenMethod = "shell.open"
const ShellProtocol = "shell-v1"

type ShellRequest struct {
	Shell string `json:"shell"`
	Term  string `json:"term,omitempty"`
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
}

type ShellDescription struct {
	Protocol string `json:"protocol"`
	Method   string `json:"method"`
	Enabled  bool   `json:"enabled"`
}

func ValidShellName(name string) bool {
	switch name {
	case "bash", "cmd", "powershell", "pwsh":
		return true
	default:
		return false
	}
}

func ValidShellDimensions(cols, rows uint16) bool {
	return cols > 0 && cols <= 32767 && rows > 0 && rows <= 32767
}

func (r ShellRequest) Validate() error {
	if !ValidShellName(r.Shell) {
		return errors.New("shell must be bash, cmd, powershell, or pwsh")
	}
	if !ValidShellDimensions(r.Cols, r.Rows) {
		return errors.New("terminal dimensions must be 1..32767")
	}
	const termChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_.+-"
	if len(r.Term) > 64 || strings.ContainsFunc(r.Term, func(c rune) bool { return !strings.ContainsRune(termChars, c) }) {
		return errors.New("terminal type must be at most 64 ASCII letters, digits, or _ . + -")
	}
	return nil
}
