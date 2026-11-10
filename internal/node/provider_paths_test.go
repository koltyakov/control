package node

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestExecAndFilePathBases(t *testing.T) {
	t.Parallel()
	n := localNode(t, nil)
	workspace := filepath.Join(n.Config.WorkDir, "tasks", "path-test")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.Config.WorkDir, "input.bin"), []byte("root"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		dir  string
		want string
	}{
		{"default workspace", "", workspace},
		{"relative task workspace", "nested", filepath.Join(workspace, "nested")},
		{"explicit sibling", filepath.Join("..", "sibling"), filepath.Join(n.Config.WorkDir, "tasks", "sibling")},
		{"absolute unchanged", t.TempDir(), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.dir
			}
			if err := os.MkdirAll(want, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(want, "input.bin"), []byte("workspace"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := executable(t, "encode")
			cfg.Dir = tc.dir
			result, err := n.execRun(t.Context(), model.JSON(cfg), Execution{Dir: workspace, Log: io.Discard})
			if err != nil || result.(commandResult).ExitCode != 0 {
				t.Fatal("exec did not use invocation-relative dir", result, err)
			}
			data, err := os.ReadFile(filepath.Join(want, "output.bin"))
			if err != nil || string(data) != "WORKSPACE" {
				t.Fatal("exec resolved wrong working directory", string(data), err)
			}
		})
	}
	result, err := n.filesRead(t.Context(), model.JSON(map[string]string{"path": "input.bin"}), Execution{Dir: workspace})
	if err != nil || result.(map[string]any)["data"] != base64.StdEncoding.EncodeToString([]byte("root")) {
		t.Fatal("files.read used task workspace instead of workDir", result, err)
	}
	for _, cap := range n.Capabilities() {
		if cap.Name != "exec.run" {
			continue
		}
		for _, fragment := range []string{"relative dir", "workDir/tasks/ID", "files.*", "absolute dir", "Declared task outputs"} {
			if !strings.Contains(cap.Description, fragment) {
				t.Errorf("exec description omits %q", fragment)
			}
		}
		var schema struct {
			Properties map[string]struct{ Description string } `json:"properties"`
		}
		if err := json.Unmarshal(cap.InputSchema, &schema); err != nil || !strings.Contains(schema.Properties["dir"].Description, "Unlike files.* paths") {
			t.Fatal("dir schema does not explain path bases", string(cap.InputSchema), err)
		}
		return
	}
	t.Fatal("exec.run capability missing")
}
