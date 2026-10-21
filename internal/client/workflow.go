package client

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/koltyakov/control/internal/model"
)

type WorkflowReconciliation struct {
	Parent   model.TaskSummary     `json:"parent"`
	Steps    []WorkflowObservation `json:"steps"`
	Complete bool                  `json:"complete"`
}

type WorkflowObservation struct {
	Name   string             `json:"name"`
	Target string             `json:"target"`
	ID     string             `json:"id"`
	Task   *model.TaskSummary `json:"task,omitempty"`
	Error  string             `json:"error,omitempty"`
}

// ReconcileWorkflow reads the persisted plan and each stable child ID using the
// account owner's authority. It never resubmits a child or restores a grant.
func (c Client) ReconcileWorkflow(ctx context.Context, target, id string) (WorkflowReconciliation, error) {
	var report WorkflowReconciliation
	var parent model.Task
	if err := c.Call(ctx, target, "tasks.get", map[string]any{"id": id}, &parent); err != nil {
		return report, err
	}
	report.Parent = model.SummarizeTask(parent)
	if parent.Pruned || parent.Spec.Capability != "workflow.run" {
		return report, errors.New("reconciliation requires a retained workflow.run task")
	}
	var plan struct {
		Steps []model.WorkflowStep `json:"steps"`
	}
	if err := json.Unmarshal(parent.Spec.Args, &plan); err != nil {
		return report, err
	}
	if len(plan.Steps) < 1 || len(plan.Steps) > 100 {
		return report, errors.New("invalid persisted workflow plan")
	}
	report.Complete = true
	for _, step := range plan.Steps {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		observation := WorkflowObservation{Name: step.Name, Target: step.Target, ID: step.Task.ID}
		var task model.Task
		if step.Task.ID == "" {
			observation.Error = "plan has no stable child ID; consult the workflow log"
		} else if err := c.Call(ctx, step.Target, "tasks.get", map[string]any{"id": step.Task.ID}, &task); err != nil {
			observation.Error = err.Error()
		} else {
			summary := model.SummarizeTask(task)
			observation.Task = &summary
		}
		if observation.Task == nil || task.State != "succeeded" {
			report.Complete = false
		}
		report.Steps = append(report.Steps, observation)
	}
	return report, nil
}
