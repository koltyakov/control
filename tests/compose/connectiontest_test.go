//go:build compose

package compose_test

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestConnectionSpeedtestCLI(t *testing.T) {
	ctx, _ := environment(t)
	for _, from := range []string{"", "source"} {
		args := []string{"speedtest", "worker", "--size", "9", "--samples", "3", "--json"}
		if from != "" {
			args = append(args, "--from", from)
		}
		var result model.ConnectionTestResult
		if err := json.Unmarshal(cli(t, ctx, args...), &result); err != nil {
			t.Fatal(err)
		}
		if result.Transport != os.Getenv("CONTROL_EXPECT_TRANSPORT") || result.Upload.Bytes != 9<<20 || result.Download.Bytes != 9<<20 || result.Latency.Samples != 3 || result.Upload.Mbps <= 0 || result.Download.Mbps <= 0 {
			t.Fatalf("invalid connection test: %+v", result)
		}
		if from != "" && result.Source != from {
			t.Fatal("wrong source", result)
		}
	}
}
