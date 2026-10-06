package clipboard

import (
	"context"
	"slices"
	"strings"
	"testing"
)

func TestClipboardWindowsCommand(t *testing.T) {
	cmd, err := command(context.Background())
	if err != nil || !slices.Contains(cmd.Args, "-STA") || !strings.Contains(cmd.Args[len(cmd.Args)-1], "$ErrorActionPreference = 'Stop'") {
		t.Fatal("clipboard PowerShell contract", cmd, err)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&0x08000000 == 0 {
		t.Fatal("clipboard command can create a console window")
	}
}
