//go:build !windows

package processutil

import "os/exec"

func HideWindow(*exec.Cmd) {}
