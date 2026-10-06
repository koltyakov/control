package main

import (
	"context"
	"testing"

	"github.com/koltyakov/control/internal/client"
)

func TestClipboardCLIArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"paste"}, {"read", "worker"}, {"paste", ""}, {"paste", "worker", "--timeout", "0s"}, {"paste", "worker", "--timeout", "25h"}, {"paste", "worker", "extra"}, {"paste", "worker", "--unknown"}} {
		if err := clipboardCLI(context.Background(), client.Client{}, args); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
}
