package main

import (
	"context"
	"errors"
	"flag"
	"os"

	"github.com/koltyakov/control/internal/installation"
)

func installAgent(ctx context.Context, agent, project, binary, config string, mcp, skill bool) error {
	return installation.InstallAgent(ctx, agent, project, binary, config, mcp, skill)
}
func agentInstallCLI(ctx context.Context, command string, args []string, config string) error {
	if len(args) == 0 {
		return errors.New("provide a client: opencode, claude, cursor, copilot, codex, windsurf, antigravity, or agents")
	}
	f := flag.NewFlagSet(command, flag.ContinueOnError)
	project := f.String("project", "", "project directory; omitted installs globally")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	return installAgent(ctx, args[0], *project, binary, config, command == "install-mcp", command == "install-skill")
}
