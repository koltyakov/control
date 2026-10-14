package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func BenchmarkMetadataWrite(b *testing.B) {
	value := model.Task{ID: "task", Owner: "owner", State: "running", Spec: model.TaskSpec{Capability: "exec.run", Args: model.JSON(map[string]any{"command": "program", "args": []string{strings.Repeat("x", 4096)}})}}
	for _, indent := range []bool{false, true} {
		name := "compact"
		if indent {
			name = "indented"
		}
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(b.TempDir(), "task.json")
			b.ReportAllocs()
			for b.Loop() {
				var data []byte
				var err error
				if indent {
					data, err = json.MarshalIndent(value, "", "  ")
				} else {
					data, err = json.Marshal(value)
				}
				if err == nil {
					err = Bytes(path, data, 0600)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
