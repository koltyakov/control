package main

import (
	"context"
	"testing"

	"github.com/koltyakov/control/internal/client"
)

func TestSpeedtestCLIArguments(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"worker", "--size", "0"}, {"worker", "--size", "257"}, {"worker", "--samples", "0"}, {"worker", "--samples", "-1"}, {"worker", "--samples", "101"}, {"worker", "--timeout", "0s"}, {"worker", "--timeout", "3m"}, {"worker", "extra"}, {"worker", "--unknown"}} {
		if err := speedtestCLI(context.Background(), client.Client{}, args); err == nil {
			t.Fatalf("accepted invalid arguments %q", args)
		}
	}
}
