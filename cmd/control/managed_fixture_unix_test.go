//go:build !windows

package main

import (
	"fmt"
	"os"
)

func testManagedFixture() bool {
	if len(os.Args) != 5 || os.Args[1] != "__test-managed-exec" {
		return false
	}
	if err := managedExecFixture(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return true
}
