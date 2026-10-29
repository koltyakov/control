package node

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// Log reads should depend on the chunk size, not the retained task payload.
func BenchmarkTaskLogPayload(b *testing.B) {
	for _, size := range []int{0, 2 << 20} {
		for _, stream := range []bool{false, true} {
			b.Run(fmt.Sprintf("payload=%d/stream=%t", size, stream), func(b *testing.B) {
				n := &Node{
					Config: Config{DataDir: b.TempDir()}, Identity: &identity.Identity{ID: "local"},
					tasks: map[string]*model.Task{"task": {ID: "task", Owner: "owner", State: "succeeded", Spec: model.TaskSpec{Args: model.JSON(strings.Repeat("a", size))}, Result: model.JSON(strings.Repeat("r", size))}},
				}
				if err := os.MkdirAll(filepath.Join(n.Config.DataDir, "tasks"), 0700); err != nil {
					b.Fatal(err)
				}
				if err := os.WriteFile(n.logPath("task"), []byte(strings.Repeat("l", 64<<10)), 0600); err != nil {
					b.Fatal(err)
				}
				args := model.JSON(map[string]any{"id": "task"})
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					var err error
					if stream {
						_, _, err = n.readTaskLog("owner", "task", 0)
					} else {
						_, err = n.taskMethod(context.Background(), "owner", "tasks.logs", args)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
