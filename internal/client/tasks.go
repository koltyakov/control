package client

import (
	"context"
	"errors"
	"fmt"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
)

// StartTask submits a tracked task under a client-chosen ID. The ID is
// assigned and validated before sending, so a lost reply can always be
// reconciled with tasks.get instead of being resubmitted. It returns the
// submitted ID with every error that follows sending.
func (c Client) StartTask(ctx context.Context, target string, spec model.TaskSpec, result any) (string, error) {
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return "", model.OperationError(fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID), "not_sent")
	}
	// Resolution failures are known outcomes, not uncertain submissions.
	r, err := c.ResolveTask(ctx, target, spec)
	if err != nil {
		return spec.ID, err
	}
	return c.StartResolvedTask(ctx, target, r, result)
}

// TaskSubmitError names the stable task ID to reconcile after a failed or
// uncertain submission. It preserves the transport outcome.
type TaskSubmitError struct {
	ID  string
	Err error
}

func (e *TaskSubmitError) Error() string {
	var rpc *model.RPCError
	if errors.As(e.Err, &rpc) && rpc.Outcome == model.OutcomeNotAccepted {
		// The worker refused before accepting; there is nothing to reconcile.
		return fmt.Sprintf("task %s was not accepted: %v", e.ID, e.Err)
	}
	return fmt.Sprintf("submit task %s; query this ID before retrying: %v", e.ID, e.Err)
}

func (e *TaskSubmitError) Unwrap() error { return e.Err }
