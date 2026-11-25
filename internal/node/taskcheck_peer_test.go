package node

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/model"
)

// A check needs the same authority as tasks.start over real peer transports:
// the tasks.start permission for the method and the capability permission.
func TestTaskCheckAuthorizationParity(t *testing.T) {
	t.Parallel()
	sourceNode, _, consumer := cluster(t, false, func(i int, cfg *Config) {
		if i == 1 {
			cfg.Allow = map[string][]string{
				"source":   {"tasks.start", "tasks.get", "files.list"},
				"consumer": {"tasks.check", "files.list"},
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	allowed := model.TaskSpec{ID: "parity", Capability: "files.list", Args: model.JSON(map[string]any{})}
	var report model.TaskCheck
	if err := testCall(t, sourceNode, ctx, "worker", model.TaskCheckMethod, allowed, &report); err != nil || !report.OK {
		t.Fatal("permitted check", report, err)
	}
	if err := testCall(t, sourceNode, ctx, "worker", "tasks.start", allowed, nil); err != nil {
		t.Fatal("permitted submission", err)
	}
	report = model.TaskCheck{}
	if err := testCall(t, sourceNode, ctx, "worker", model.TaskCheckMethod, allowed, &report); err != nil || checkItem(t, report, "id").Status != model.TaskCheckPass || !strings.Contains(checkItem(t, report, "id").Message, "already exists") {
		t.Fatal("check after acceptance", report, err)
	}
	denied := model.TaskSpec{ID: "parity", Capability: "exec.run", Args: model.JSON(map[string]any{"command": "true"})}
	report = model.TaskCheck{}
	if err := testCall(t, sourceNode, ctx, "worker", model.TaskCheckMethod, denied, &report); err != nil {
		t.Fatal(err)
	}
	wantCheck(t, report, "authorization", model.TaskCheckFail, "not authorized for exec.run")
	for _, name := range []string{"id", "args", "inputs", "outputs", "lease", "admission"} {
		// Without capability authority the check reveals nothing about the ID
		// already used above, leases or admission.
		wantCheck(t, report, name, model.TaskCheckSkip, "requires authority")
	}
	if len(report.Pools) != 0 {
		t.Fatal("unauthorized check reported pools", report.Pools)
	}
	if err := testCall(t, sourceNode, ctx, "worker", "tasks.start", model.TaskSpec{ID: "denied", Capability: "exec.run", Args: denied.Args}, nil); err == nil || !strings.Contains(err.Error(), "not authorized for exec.run") {
		t.Fatal("submission was not refused like its check", err)
	}
	// A tasks.check rule alone grants nothing; the refusal names tasks.start
	// and is not mistaken for an older worker.
	for _, method := range []string{model.TaskCheckMethod, "tasks.start"} {
		err := testCall(t, consumer, ctx, "worker", method, allowed, nil)
		if err == nil || !strings.Contains(err.Error(), "not authorized for tasks.start") || strings.Contains(err.Error(), "preflight not supported") {
			t.Fatalf("%s without tasks.start permission: %v", method, err)
		}
	}
}

// On an instruction-only Hybrid System companion the shared client prepares a
// separate exact single-use check grant and revokes it. It never uses or
// replaces the grant of an accepted task, and coordination checks are refused
// at grant issuance exactly as their submissions are.
func TestTaskCheckInstructionOnlyDestination(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			t.Parallel()
			f := newLifetimeFixture(t, relay, true, nil)
			if grants := f.grants(t, "second"); len(grants) != 0 {
				t.Fatal("unexpected grants", grants)
			}
			spec := lifetimeTask("system-checked", true)
			report, err := f.client.CheckTask(f.ctx, "second", spec)
			if err != nil || !report.OK {
				t.Fatal("System check", report, err)
			}
			if grants := f.grants(t, "second"); len(grants) != 0 {
				t.Fatal("check grant outlived its call", grants)
			}
			if err = f.client.Call(f.ctx, "second", "tasks.start", spec, nil); err != nil {
				t.Fatal(err)
			}
			f.waitEntered(t)
			before := f.grants(t, "second")
			if len(before) != 1 || before[0].Method != "tasks.start" {
				t.Fatal("submission grant", before)
			}
			report, err = f.client.CheckTask(f.ctx, "second", spec)
			if err != nil || !strings.Contains(checkItem(t, report, "id").Message, "already exists") {
				t.Fatal("System check of the accepted task", report, err)
			}
			if after := f.grants(t, "second"); !reflect.DeepEqual(grantIDs(before), grantIDs(after)) {
				t.Fatal("check changed the submission grant", before, after)
			}
			if _, err = f.client.CheckTask(f.ctx, "second", lifetimeWorkflow()); err == nil || !strings.Contains(err.Error(), "cannot create further coordination") {
				t.Fatal("System workflow check was not refused like its submission", err)
			}
			f.release()
			task, err := f.client.Wait(f.ctx, "second", spec.ID)
			if err != nil || task.State != "succeeded" || f.calls.Load() != 1 {
				t.Fatal("checks affected execution", task, err, f.calls.Load())
			}
		})
	}
}

func grantIDs(grants []model.Delegation) []string {
	ids := make([]string, len(grants))
	for i, grant := range grants {
		ids[i] = grant.ID + grant.Method
	}
	return ids
}
