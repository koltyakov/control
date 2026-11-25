package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

const (
	// defaultTaskReserveBytes matches the artifact and upload reserves.
	defaultTaskReserveBytes = 256 << 20
	// diskGuardMaxAge bounds the age of a reading used for an admission
	// decision. An older reading is unknown and never refuses work.
	diskGuardMaxAge = 2 * time.Second
	// diskGuardProbeWait bounds how long admission waits for a probe. A probe
	// that has not finished by then leaves the decision unknown.
	diskGuardProbeWait = time.Second
	// diskInsufficientCode identifies a refusal caused by the task reserve.
	diskInsufficientCode = "insufficient_disk_space"
)

// diskFreeReader returns the bytes available to this process on the
// filesystem holding path.
type diskFreeReader func(path string) (uint64, error)

// diskGuard caches one free-space reading of the task workspace root and node
// metadata filesystems. At most one probe runs at a time, outside n.mu.
type diskGuard struct {
	mu      sync.Mutex
	reading diskReading
	probing chan struct{} // Closed when the in-flight probe publishes.
	// free and now are nil in production. Tests inject them before use.
	free diskFreeReader
	now  func() time.Time
}

type diskReading struct {
	at   time.Time // Probe start; zero before the first probe.
	work diskPathReading
	data diskPathReading
}

type diskPathReading struct {
	free uint64
	err  error
}

func (c *Config) taskReserveDefaults() error {
	if c.TaskReserveBytes == 0 {
		c.TaskReserveBytes = defaultTaskReserveBytes
	}
	if c.TaskReserveBytes < -1 {
		return errors.New("taskReserveBytes must be -1 or positive; zero selects the 256 MiB default and -1 disables the task disk guard")
	}
	return nil
}

func (g *diskGuard) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// observe returns a reading no older than diskGuardMaxAge when one can be
// obtained within diskGuardProbeWait, otherwise the latest reading, which the
// caller treats as unknown when stale. A probe is never cancelled: statfs and
// GetDiskFreeSpaceEx cannot be, so a hung filesystem leaves one goroutine
// waiting in the system call rather than stalling admission.
func (g *diskGuard) observe(ctx context.Context, probe func() diskReading) diskReading {
	g.mu.Lock()
	if r := g.reading; !r.at.IsZero() && g.clock().Sub(r.at) <= diskGuardMaxAge {
		g.mu.Unlock()
		return r
	}
	wait := g.probing
	if wait == nil {
		wait = make(chan struct{})
		g.probing = wait
		go func() {
			r := probe()
			g.mu.Lock()
			g.reading, g.probing = r, nil
			g.mu.Unlock()
			close(wait)
		}()
	}
	g.mu.Unlock()
	timer := time.NewTimer(diskGuardProbeWait)
	defer timer.Stop()
	select {
	case <-wait:
	case <-timer.C:
	case <-ctx.Done():
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reading
}

// probeTaskDisk reads both filesystems. The workspace root may not exist until
// the first task; its parent workDir is then on the same filesystem.
func (n *Node) probeTaskDisk() diskReading {
	free := n.taskDisk.free
	if free == nil {
		free = availableDiskBytes
	}
	r := diskReading{at: n.taskDisk.clock()}
	work := filepath.Join(n.Config.WorkDir, "tasks")
	if _, err := os.Lstat(work); err != nil {
		work = n.Config.WorkDir
	}
	r.work.free, r.work.err = free(work)
	r.data.free, r.data.err = free(n.Config.DataDir)
	return r
}

// taskDiskState reports whether new work is currently refused for low disk
// space. state is disabled, unknown, low or ok. Unknown never refuses.
func (n *Node) taskDiskState(ctx context.Context) (state string, reading diskReading, err error) {
	reserve := n.Config.TaskReserveBytes
	if reserve < 0 {
		return "disabled", diskReading{}, nil
	}
	reading = n.taskDisk.observe(ctx, n.probeTaskDisk)
	if reading.at.IsZero() || n.taskDisk.clock().Sub(reading.at) > diskGuardMaxAge {
		return "unknown", reading, nil
	}
	state = "ok"
	for _, p := range []struct {
		name string
		r    diskPathReading
	}{{"task workspace", reading.work}, {"node data", reading.data}} {
		if p.r.err != nil {
			state = "unknown"
			continue
		}
		if p.r.free < uint64(reserve) {
			return "low", reading, fmt.Errorf("%s filesystem has %d bytes free, below taskReserveBytes %d", p.name, p.r.free, reserve)
		}
	}
	return state, reading, nil
}

// admitTaskDisk refuses new work below the reserve. what names the refused
// request for the message. It reads cached state and never takes n.mu.
func (n *Node) admitTaskDisk(ctx context.Context, what string) error {
	_, _, low := n.taskDiskState(ctx)
	if low == nil {
		return nil
	}
	return &model.RPCError{Code: diskInsufficientCode, Outcome: model.OutcomeNotAccepted, Message: what + " refused for low disk space; nothing was recorded or run: " + low.Error() + "; free space on this worker (delete unneeded artifacts, uploads or files) or choose another machine, for example nodes.select with minDiskBytes, then submit again", Cause: low}
}

// diskGuardedCall reports synchronous built-ins whose purpose is adding bytes
// to the workspace. Removal, moves and arbitrary commands stay available as
// recovery routes; artifact and upload writes keep their own reserves.
func diskGuardedCall(method string) bool {
	return method == "files.write" || method == "files.copy"
}

// taskDiskDiagnostics reports the reserve, the latest reading and whether
// admission is refused. Errors are sanitized so local paths never appear.
func (n *Node) taskDiskDiagnostics(ctx context.Context) map[string]any {
	state, reading, _ := n.taskDiskState(ctx)
	report := map[string]any{"reserveBytes": n.Config.TaskReserveBytes, "state": state, "refusing": state == "low", "maxAgeSeconds": int(diskGuardMaxAge / time.Second)}
	if reading.at.IsZero() {
		return report
	}
	report["checkedAt"] = reading.at.UTC()
	path := func(r diskPathReading, dirs ...string) map[string]any {
		if r.err != nil {
			message := r.err.Error()
			for _, dir := range dirs {
				message = strings.ReplaceAll(message, dir, "<dir>")
			}
			return map[string]any{"error": message}
		}
		return map[string]any{"freeBytes": r.free}
	}
	report["work"] = path(reading.work, filepath.Join(n.Config.WorkDir, "tasks"), n.Config.WorkDir)
	report["data"] = path(reading.data, n.Config.DataDir)
	return report
}
