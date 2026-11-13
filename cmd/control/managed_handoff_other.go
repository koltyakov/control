//go:build !windows

package main

import (
	"context"
	"errors"
)

func runManagedHandoff(context.Context, ...string) error {
	return errors.New("temporary runtime handoff is only needed on Windows")
}
