//go:build !windows

package main

import "context"

func runPlatformService(context.Context, []string) (bool, error) { return false, nil }
