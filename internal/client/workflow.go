package client

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

type WorkflowReconciliation struct {
	Parent   model.TaskSummary     `json:"parent"`
	Steps    []WorkflowObservation `json:"steps"`
	Complete bool                  `json:"complete"`
}

// NotStarted means the terminal coordinator recorded no child for the step and
// its destination reports no such task. When echoes the plan's step condition.
type WorkflowObservation struct {
	Name           string             `json:"name"`
	Target         string             `json:"target"`
	ID             string             `json:"id"`
	When           string             `json:"when,omitempty"`
	Task           *model.TaskSummary `json:"task,omitempty"`
	NotStarted     bool               `json:"notStarted,omitempty"`
	Error          string             `json:"error,omitempty"`
	Code           string             `json:"code,omitempty"`
	Outcome        string             `json:"outcome,omitempty"`
	ErrorTruncated bool               `json:"errorTruncated,omitempty"`
}

// workerTaskNotFound is the destination's reply for an ID it never accepted.
// Pruned tasks keep tombstones and are reported as pruned instead.
const workerTaskNotFound = "task not found"

// ReconcileWorkflow reads the persisted plan and each stable child ID using the
// account owner's authority. It never resubmits a child or restores a grant.
func (c Client) ReconcileWorkflow(ctx context.Context, target, id string) (WorkflowReconciliation, error) {
	return c.reconcileWorkflow(ctx, target, id, nil)
}

// reconcileWorkflow optionally reports, after each child query, how many
// children were queried and how many are terminal. progress must not block.
func (c Client) reconcileWorkflow(ctx context.Context, target, id string, progress func(queried, terminal, total int)) (WorkflowReconciliation, error) {
	var report WorkflowReconciliation
	observe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var parent model.Task
	if err := c.Call(observe, target, "tasks.get", map[string]any{"id": id}, &parent); err != nil {
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
	// A terminal coordinator's retained result lists every child it observed.
	// Uncertain submissions are absent too, so absence alone proves nothing.
	var recorded map[string]json.RawMessage
	if !parent.Terminal() || len(parent.Result) == 0 || json.Unmarshal(parent.Result, &recorded) != nil {
		recorded = nil
	}
	report.Steps = make([]WorkflowObservation, len(plan.Steps))
	jobs := make(chan int, len(plan.Steps))
	for i, step := range plan.Steps {
		report.Steps[i] = WorkflowObservation{Name: step.Name, Target: step.Target, ID: step.Task.ID, When: step.When}
		jobs <- i
	}
	close(jobs)
	var counted sync.Mutex
	queried, terminal := 0, 0
	count := func(task *model.TaskSummary) {
		if progress == nil {
			return
		}
		counted.Lock()
		defer counted.Unlock()
		queried++
		if task != nil && (model.Task{State: task.State}).Terminal() {
			terminal++
		}
		progress(queried, terminal, len(plan.Steps))
	}
	var wg sync.WaitGroup
	for range min(8, len(plan.Steps)) {
		wg.Go(func() {
			for i := range jobs {
				observation := &report.Steps[i]
				if observation.ID == "" {
					observation.Error = "plan has no stable child ID; consult the workflow log"
					count(nil)
					continue
				}
				poll, done := context.WithTimeout(observe, 6*time.Second)
				var task model.Task
				err := poll.Err()
				if err == nil {
					err = c.Call(poll, observation.Target, "tasks.get", map[string]any{"id": observation.ID}, &task)
				}
				done()
				if err != nil {
					rpc := model.OperationError(err, "unknown")
					observation.Error, observation.Code, observation.Outcome = rpc.Message, rpc.Code, rpc.Outcome
					if _, ok := recorded[observation.Name]; recorded != nil && !ok && rpc.Cause == nil && rpc.Message == workerTaskNotFound {
						observation.NotStarted = true
					}
					if len(observation.Error) > 1024 {
						observation.Error = strings.Clone(observation.Error[:1024])
						observation.ErrorTruncated = true
					}
					count(nil)
					continue
				}
				summary := model.SummarizeTask(task)
				observation.Task = &summary
				count(&summary)
			}
		})
	}
	wg.Wait()
	report.Complete = true
	for _, observation := range report.Steps {
		if observation.Task == nil || observation.Task.State != "succeeded" {
			report.Complete = false
		}
	}
	// Internal observation deadlines leave useful partial reports. Explicit
	// caller cancellation also returns its cause after every query has stopped.
	return report, ctx.Err()
}
