package main

import (
	"context"
	"testing"

	"github.com/koltyakov/control/internal/client"
)

func TestWakeCLIValidation(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"worker", "extra"}, {"worker", "--unknown"}, {"worker", "--from", "local"}, {"worker", "--lan", "home"}} {
		if err := wakeCLI(context.Background(), client.Client{}, args); err == nil {
			t.Fatal("accepted invalid wake arguments", args)
		}
	}
}
