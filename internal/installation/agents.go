package installation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/skills"
	"github.com/pelletier/go-toml/v2"
	"github.com/tailscale/hujson"
)

type AgentPaths struct {
	MCP, Skill string
	ServerPath []string
	TOML       bool
}

func AgentLocations(agent, home, project string) (AgentPaths, error) {
	global := project == ""
	base := home
	if !global {
		base = project
	}
	var config, skill string
	server := []string{"mcpServers"}
	switch agent {
	case "opencode":
		if global {
			base = filepath.Join(home, ".config")
			if value := os.Getenv("XDG_CONFIG_HOME"); value != "" {
				base = value
			}
			config = "opencode/opencode.json"
			skill = "opencode/skills/control"
		} else {
			config = "opencode.json"
			skill = ".opencode/skills/control"
		}
		server = []string{"mcp", "servers"}
	case "claude":
		config = ".mcp.json"
		if global {
			config = ".claude.json"
		}
		skill = ".claude/skills/control"
	case "cursor":
		config = ".cursor/mcp.json"
		skill = ".cursor/skills/control"
	case "copilot":
		config = ".copilot/mcp-config.json"
		skill = ".copilot/skills/control"
		if !global {
			config = ".vscode/mcp.json"
			skill = ".github/skills/control"
			server = []string{"servers"}
		}
	case "codex":
		config = ".codex/config.toml"
		skill = ".codex/skills/control"
	case "windsurf":
		config = ".codeium/windsurf/mcp_config.json"
		skill = ".codeium/windsurf/skills/control"
		if !global {
			config = ".windsurf/mcp_config.json"
			skill = ".windsurf/skills/control"
		}
	case "antigravity":
		config = ".gemini/antigravity/mcp_config.json"
		skill = ".gemini/antigravity/skills/control"
		if !global {
			config = ".agents/mcp_config.json"
			skill = ".agents/skills/control"
		}
	case "agents":
		config = ".agents/mcp.json"
		skill = ".agents/skills/control"
	default:
		return AgentPaths{}, errors.New("client must be opencode, claude, cursor, copilot, codex, windsurf, antigravity, or agents")
	}
	return AgentPaths{MCP: filepath.Join(base, filepath.FromSlash(config)), Skill: filepath.Join(base, filepath.FromSlash(skill), "SKILL.md"), ServerPath: server, TOML: agent == "codex"}, nil
}

func InstallAgent(ctx context.Context, agent, project, binary, config string, mcp, skill bool) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	paths, err := AgentLocations(agent, home, project)
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	config, err = filepath.Abs(config)
	if err != nil {
		return err
	}
	args := []string{"--config", config, "mcp"}
	if mcp {
		// OpenCode's native command preserves JSONC and unrelated configuration.
		if cli, err := exec.LookPath("opencode"); agent == "opencode" && err == nil {
			command := []string{"mcp", "add", "control"}
			if project == "" {
				command = append(command, "--global")
			}
			command = append(command, "--", binary)
			command = append(command, args...)
			cmd := exec.CommandContext(ctx, cli, command...)
			cmd.Dir = project
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if err = cmd.Run(); err != nil {
				return err
			}
		} else {
			entry := map[string]any{"command": binary, "args": args, "type": "stdio"}
			if agent == "opencode" {
				entry = map[string]any{"type": "local", "command": append([]string{binary}, args...)}
			}
			if paths.TOML {
				delete(entry, "type")
			}
			if err = mergeMCP(paths, entry); err != nil {
				return err
			}
		}
		fmt.Fprintln(os.Stderr, "MCP configured for", agent)
	}
	if skill {
		if err = writeManaged(paths.Skill, skills.Control); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Skill installed:", paths.Skill)
	}
	return nil
}

func mergeMCP(paths AgentPaths, entry map[string]any) error {
	path := paths.MCP
	if !paths.TOML {
		if _, err := os.Stat(path + "c"); err == nil {
			path += "c"
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	lock := flock.New(path + ".control.lock")
	held, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !held {
		return errors.New("client configuration is being edited")
	}
	defer lock.Close()
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var content []byte
	if paths.TOML {
		root := map[string]any{}
		if len(original) > 0 {
			if err = toml.Unmarshal(original, &root); err != nil {
				return err
			}
		}
		servers, ok := root["mcp_servers"].(map[string]any)
		if !ok {
			if root["mcp_servers"] != nil {
				return errors.New("mcp_servers is not a table")
			}
			servers = map[string]any{}
			root["mcp_servers"] = servers
		}
		servers["control"] = entry
		content, err = toml.Marshal(root)
		if err != nil {
			return err
		}
	} else {
		input := original
		if len(input) == 0 {
			input = []byte("{}")
		}
		value, err := hujson.Parse(input)
		if err != nil {
			return err
		}
		pointer := ""
		for _, part := range paths.ServerPath {
			pointer += "/" + part
			if value.Find(pointer) == nil {
				patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": pointer, "value": map[string]any{}}})
				if err = value.Patch(patch); err != nil {
					return err
				}
			}
		}
		patch, _ := json.Marshal([]map[string]any{{"op": "add", "path": pointer + "/control", "value": entry}})
		if err = value.Patch(patch); err != nil {
			return err
		}
		value.Format()
		content = value.Pack()
	}
	if len(original) > 0 && string(original) != string(content) {
		backup := path + ".control-backup"
		if _, err = os.Stat(backup); os.IsNotExist(err) {
			if err = store.Bytes(backup, original, 0600); err != nil {
				return err
			}
		}
	}
	return store.Bytes(path, content, 0600)
}
