package client

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/skills"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// undocumentedMCPTools lists tools not yet named in docs/, README.md or the
// shipped skill bundle. Their own descriptions and the MCP server
// instructions are currently their only documentation. Remove an entry when
// documenting the tool; the test fails while a documented tool stays listed.
var undocumentedMCPTools = map[string]string{
	"control_access_list":      "access.list is in docs/protocol.md; the tool is named only in the MCP server instructions",
	"control_access_revoke":    "access.revoke is in docs/protocol.md; the tool is named only in the MCP server instructions",
	"control_artifact_deliver": "artifacts.deliver is in docs/protocol.md; the tool has only its own description",
	"control_mcp_call":         "mcp.call is in docs/protocol.md; the tool has only its own description",
	"control_mcp_discover":     "mcp.discover is in docs/protocol.md; the tool has only its own description",
}

// toolMention matches a tool name, optionally followed by slash-separated
// suffixes such as control_forward_start/list/stop.
var toolMention = regexp.MustCompile(`control_[a-z_]*[a-z]((?:/[a-z]+)*)`)

// documentedToolNames returns every control_* name mentioned in text,
// expanding control_X_start/list/stop into control_X_start, _list and _stop.
func documentedToolNames(text string, names map[string]bool) {
	for _, match := range toolMention.FindAllStringSubmatch(text, -1) {
		name := strings.TrimSuffix(match[0], match[1])
		names[name] = true
		if match[1] == "" {
			continue
		}
		stem := name[:strings.LastIndex(name, "_")+1]
		for _, suffix := range strings.Split(strings.TrimPrefix(match[1], "/"), "/") {
			names[stem+suffix] = true
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
			if first, _, _ := strings.Cut(string(data), "\n"); strings.TrimSpace(first) == "module github.com/koltyakov/control" {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repository root with go.mod not found")
		}
		dir = parent
	}
}

// TestMCPToolsAreDocumented fails when a registered MCP tool is named nowhere
// in the documentation, README, shipped skill bundle or on-demand guides.
// The export tool's description matches artifacts.export: workspace-relative
// paths, and directories exported as one tar archive.
func TestMCPArtifactExportDescribesDirectories(t *testing.T) {
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{}).MCPServer().Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Name != "control_artifact_export" {
			continue
		}
		for _, want := range []string{"file or directory", "workDir", ".tar", "tar-v1"} {
			if !strings.Contains(tool.Description, want) {
				t.Fatalf("description lacks %q: %s", want, tool.Description)
			}
		}
		if strings.Contains(tool.Description, "filesystem root") {
			t.Fatal("description still names the filesystem root", tool.Description)
		}
		return
	}
	t.Fatal("control_artifact_export not listed")
}

func TestMCPToolsAreDocumented(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := (Client{}).MCPServer().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = serverSession.Close() }()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 30 {
		t.Fatalf("listed only %d MCP tools", len(tools.Tools))
	}

	documented := map[string]bool{}
	root := repositoryRoot(t)
	texts := []string{desktopGuide}
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	texts = append(texts, string(readme))
	err = filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".md") {
			return err
		}
		data, err := os.ReadFile(path)
		texts = append(texts, string(data))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	err = fs.WalkDir(skills.ControlFiles, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := skills.ControlFiles.ReadFile(path)
		texts = append(texts, string(data))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range texts {
		documentedToolNames(text, documented)
	}

	var missing []string
	registered := map[string]bool{}
	for _, tool := range tools.Tools {
		registered[tool.Name] = true
		_, gap := undocumentedMCPTools[tool.Name]
		switch {
		case documented[tool.Name] && gap:
			t.Errorf("%s is documented now; remove it from undocumentedMCPTools", tool.Name)
		case !documented[tool.Name] && !gap:
			missing = append(missing, tool.Name)
		}
	}
	if len(missing) != 0 {
		sort.Strings(missing)
		t.Errorf("MCP tools not mentioned in docs/, README.md or the skill bundle: %s", strings.Join(missing, ", "))
	}
	for name := range undocumentedMCPTools {
		if !registered[name] {
			t.Errorf("%s is no longer registered; remove it from undocumentedMCPTools", name)
		}
	}
}
