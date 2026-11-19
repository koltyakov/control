package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/koltyakov/control/internal/installation"
)

func installSelfCLI(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("install-self", flag.ContinueOnError)
	refresh := f.Bool("refresh-skills", false, "refresh existing Control-managed global skills for other supported clients")
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if f.NArg() != 0 {
		return errors.New("install-self does not take positional arguments")
	}
	path, err := installation.InstallSelf(ctx)
	if err != nil {
		return err
	}
	fmt.Println("Installed", path)
	return installation.InstallHostSkills(ctx, *refresh)
}

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
