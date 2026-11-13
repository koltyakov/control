package main

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"

	"github.com/koltyakov/control/internal/buildinfo"
)

func testManagedFixture() bool {
	if len(os.Args) == 3 && os.Args[1] == "__test-gui-source" {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if err := buildinfo.UseWindowsGUISource(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		runtime.ReadMemStats(&after)
		actual, err := buildinfo.ExecutableSHA256()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			buildinfo.Info
			ActualSHA256             string
			ValidationAllocatedBytes uint64
		}{buildinfo.Current(), actual, after.TotalAlloc - before.TotalAlloc})
		return true
	}
	if len(os.Args) != 5 || os.Args[1] != "__test-managed-exec" {
		return false
	}
	if err := managedExecFixture(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	return true
}
