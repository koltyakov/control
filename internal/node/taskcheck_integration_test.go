package node

import (
	"context"
	"strings"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

// TestTaskCheckDigestMatchesTaggedAcceptance keeps checkedTaskSpec in step
// with the tag normalization of startTaskContext: a check of an accepted
// specification whose tags are reordered or repeated must still match it.
func TestTaskCheckDigestMatchesTaggedAcceptance(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, nil)
	spec := checkSpec("digest-tags", `{"n":1}`)
	spec.Tags = []string{"zeta", "alpha", "zeta"}
	accepted, err := n.startTaskContext(context.Background(), n.Identity.ID, spec)
	if err != nil {
		t.Fatal(err)
	}
	_, digest, err := checkedTaskSpec(spec)
	if err != nil || digest != specDigest(accepted.Spec) {
		t.Fatalf("check digest %s differs from acceptance %s: %v", digest, specDigest(accepted.Spec), err)
	}
	invalid := checkSpec("digest-bad-tag", `{"n":1}`)
	invalid.Tags = []string{"not a tag"}
	if _, _, err := checkedTaskSpec(invalid); err == nil {
		t.Fatal("check accepted a tag that tasks.start rejects")
	}
}

// TestTaskCheckReportsLowDisk verifies that a check reports the low-disk
// refusal tasks.start would return, and passes again once space is freed.
func TestTaskCheckReportsLowDisk(t *testing.T) {
	t.Parallel()
	n, _, _ := checkNode(t, func(cfg *Config) { cfg.TaskReserveBytes = 1 << 20 })
	disk := installFakeDisk(n, 1<<10, 1<<30)
	admission := func() model.TaskCheckItem {
		t.Helper()
		report := checkLocal(t, n, checkSpec("low-disk", `{"n":1}`))
		for _, item := range report.Checks {
			if item.Name == "admission" {
				if (item.Status == model.TaskCheckFail) == report.OK {
					t.Fatalf("ok=%v disagrees with admission %q", report.OK, item.Status)
				}
				return item
			}
		}
		t.Fatal("no admission check reported")
		return model.TaskCheckItem{}
	}
	item := admission()
	if item.Status != model.TaskCheckFail || !strings.Contains(item.Message, "taskReserveBytes") {
		t.Fatalf("want low-disk failure, got %+v", item)
	}
	if strings.Contains(item.Message, n.Config.WorkDir) || strings.Contains(item.Message, n.Config.DataDir) {
		t.Fatalf("message exposes a local path: %q", item.Message)
	}
	disk.mu.Lock()
	disk.work = 1 << 30
	disk.clock = disk.clock.Add(diskGuardMaxAge + 1)
	disk.mu.Unlock()
	if item := admission(); item.Status != model.TaskCheckPass {
		t.Fatalf("want pass after space was freed, got %+v", item)
	}
}
