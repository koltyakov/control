package commandexec

import (
	"context"
	"encoding/json"
	"os"
	"runtime"
	"testing"
	"time"
)

const (
	burnCPU    = 400 * time.Millisecond
	burnMemory = 96 << 20
)

var burnSink int

// burnFixture touches every page of burnMemory and spins until this process
// has used burnCPU, measured by the OS rather than by wall time, so a busy
// shared test machine cannot shorten the work.
func burnFixture() {
	memory := make([]byte, burnMemory)
	for i := 0; i < len(memory); i += 4096 {
		memory[i] = 1
	}
	deadline := time.Now().Add(30 * time.Second)
	for selfCPU() < burnCPU && time.Now().Before(deadline) {
		for i := range 1 << 20 {
			burnSink += i
		}
	}
	runtime.KeepAlive(memory)
	os.Exit(0)
}

func TestRunWithUsageMeasuresProcessTree(t *testing.T) {
	// burn-parent uses little itself; reaching the bounds requires its
	// waited child's CPU and resident set.
	for _, mode := range []string{"burn", "burn-parent"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			exit, usage, err := RunWithUsage(ctx, fixtureCommand(ctx, mode, ""))
			if err != nil || exit != 0 || usage == nil {
				t.Fatalf("exit=%d err=%v usage=%v", exit, err, usage)
			}
			encoded, _ := json.Marshal(usage)
			t.Logf("%s usage: %s", mode, encoded)
			if usage.WallMs == nil || *usage.WallMs < 50 || *usage.WallMs > 60_000 {
				t.Fatalf("wall time out of bounds: %s", encoded)
			}
			if usage.UserCPUMs == nil || usage.SystemCPUMs == nil {
				t.Fatalf("CPU time unknown: %s", encoded)
			}
			// The fixture measured burnCPU itself; floor division loses <1 ms.
			cpu := *usage.UserCPUMs + *usage.SystemCPUMs
			if cpu < burnCPU.Milliseconds()-5 || cpu > 120_000 {
				t.Fatalf("CPU time out of bounds: %s", encoded)
			}
			memory := usage.MaxRSSBytes
			absent := usage.PeakJobMemoryBytes
			if runtime.GOOS == "windows" {
				memory, absent = usage.PeakJobMemoryBytes, usage.MaxRSSBytes
			}
			if absent != nil {
				t.Fatalf("reported another platform's memory field: %s", encoded)
			}
			if memory == nil || *memory < burnMemory*4/5 || *memory > 16<<30 {
				t.Fatalf("peak memory out of bounds: %s", encoded)
			}
		})
	}
}

func TestRunWithUsageKeepsExitStatus(t *testing.T) {
	exit, usage, err := RunWithUsage(t.Context(), fixtureCommand(t.Context(), "exit", ""))
	if exit != 23 || err == nil || usage == nil || usage.WallMs == nil || usage.UserCPUMs == nil {
		t.Fatalf("exit=%d err=%v usage=%+v", exit, err, usage)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if exit, usage, err := RunWithUsage(ctx, fixtureCommand(ctx, "echo", "")); exit != -1 || err == nil || usage != nil {
		t.Fatalf("never-started command reported usage: %d %v %+v", exit, err, usage)
	}
}

func TestUsageConversion(t *testing.T) {
	m := rusageReading(1_500_000, 2_999_999, 1000, 1024)
	if *m.UserNs != 1_500_000 || *m.SystemNs != 2_999_999 || *m.MaxRSSBytes != 1_024_000 || m.PeakJobMemoryBytes != nil {
		t.Fatal("Linux kilobyte conversion", m)
	}
	if m := rusageReading(0, -1, 4096, 1); *m.UserNs != 0 || m.SystemNs != nil || *m.MaxRSSBytes != 4096 {
		t.Fatal("macOS byte units or invalid CPU", m)
	}
	for _, rss := range []int64{0, -1, 1 << 62} {
		if m := rusageReading(0, 0, rss, 1024); m.MaxRSSBytes != nil {
			t.Fatal("absent or overflowing ru_maxrss became a reading", rss)
		}
	}
	if filetimeTicks(1, 2) != 1<<32|2 || filetimeTicks(0, 0xffffffff) != 0xffffffff {
		t.Fatal("FILETIME join")
	}
	job := jobAccounting{cpuKnown: true, user: 50, kernel: 10, helperKnown: true, helperUser: 20, helperKernel: 30, peakKnown: true, peak: 8 << 20}.reading()
	if *job.UserNs != 3000 || *job.SystemNs != 0 || *job.PeakJobMemoryBytes != 8<<20 || job.MaxRSSBytes != nil {
		t.Fatal("job accounting subtracts the helper and clamps at zero", job)
	}
	for _, j := range []jobAccounting{{cpuKnown: true, user: 1}, {helperKnown: true}, {peakKnown: true}, {cpuKnown: true, helperKnown: true, user: -1}} {
		if m := j.reading(); m.UserNs != nil || m.SystemNs != nil || m.PeakJobMemoryBytes != nil {
			t.Fatal("partial job reading became known", j)
		}
	}
	if commandUsage(-1, nil) != nil || commandUsage(-1, &measured{}) != nil {
		t.Fatal("nothing known must be nil")
	}
	// A measured zero is a reading; unknown fields are omitted, never zero.
	encoded, _ := json.Marshal(commandUsage(-1, &measured{UserNs: known(999_999), SystemNs: known(-5)}))
	if string(encoded) != `{"userCpuMs":0}` {
		t.Fatal("unknown or zero readings misreported", string(encoded))
	}
	encoded, _ = json.Marshal(commandUsage(1500*time.Millisecond, nil))
	if string(encoded) != `{"wallMs":1500}` {
		t.Fatal("wall-only usage", string(encoded))
	}
	// The helper cannot claim Windows job memory over the status channel.
	var status outcome
	if json.Unmarshal([]byte(`{"exitCode":0,"usage":{"userNs":1,"peakJobMemoryBytes":5}}`), &status) != nil || status.Usage.PeakJobMemoryBytes != nil || *status.Usage.UserNs != 1 {
		t.Fatal("helper status decoding", status.Usage)
	}
}
