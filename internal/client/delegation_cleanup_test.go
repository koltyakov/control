package client

import (
	"testing"

	"github.com/koltyakov/control/internal/model"
)

func TestDelegationCleanupLifetimePolicy(t *testing.T) {
	task := model.TaskSpec{ID: "parent", Capability: "peers.call"}
	grants := []model.Delegation{{ID: "single-use", Method: "exec.run"}, {ID: "durable", Method: "tasks.start"}}
	for _, status := range []string{"accepted", "existing", "rejected", ""} {
		t.Run(status, func(t *testing.T) {
			var receipt *model.TaskSubmissionReceipt
			if status != "" {
				receipt = &model.TaskSubmissionReceipt{TaskID: task.ID, Status: status}
			}
			got := releasableDelegations("tasks.start", model.JSON(task), grants, receipt)
			want := 0
			if status == "existing" || status == "rejected" {
				want = len(grants)
			}
			if len(got) != want {
				t.Fatal("incorrect durable cleanup", status, got)
			}
		})
	}
	wrong := &model.TaskSubmissionReceipt{TaskID: "another-task", Status: "existing"}
	if got := releasableDelegations("tasks.start", model.JSON(task), grants, wrong); len(got) != 0 {
		t.Fatal("another task's receipt withdrew authority", got)
	}
	if got := releasableDelegations("workflow.run", nil, grants, nil); len(got) != 1 || got[0].ID != "single-use" {
		t.Fatal("synchronous return withdrew durable child grants", got)
	}
	forwarded := model.JSON(map[string]any{"method": "tasks.start", "params": task})
	if got := releasableDelegations("peers.call", forwarded, grants, nil); len(got) != 1 {
		t.Fatal("uncertain forwarded child lost task authority", got)
	}
}
