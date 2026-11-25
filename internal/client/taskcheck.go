package client

import (
	"context"
	"errors"
	"strings"

	"github.com/koltyakov/control/internal/model"
)

// CheckTask asks target whether tasks.start would accept spec, without
// submitting it (D181). The answer is advisory: it reserves nothing, and it is
// never evidence about whether a later submission was sent or accepted.
// Instruction-only destinations receive a separate exact, single-use
// tasks.check grant prepared by the shared client; it cannot authorize the
// submission and the submission's own grant is never used for a check.
func (c Client) CheckTask(ctx context.Context, target string, spec model.TaskSpec) (model.TaskCheck, error) {
	var check model.TaskCheck
	err := c.Call(ctx, target, model.TaskCheckMethod, spec, &check)
	return check, err
}

// taskCheckUnsupported is the code of the error returned when a node predates
// tasks.check.
const taskCheckUnsupported = "task_check_unsupported"

// taskCheckError names an older node's rejection of the unknown method.
// Current nodes never produce these messages for tasks.check: the method is
// dispatched, and its access rules are reported as tasks.start permission.
func taskCheckError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if !strings.Contains(message, `unknown method or capability "tasks.check"`) && !strings.Contains(message, "caller is not authorized for tasks.check") {
		return err
	}
	outcome := "unknown"
	var rpc *model.RPCError
	if errors.As(err, &rpc) && rpc.Outcome != "" {
		outcome = rpc.Outcome
	}
	return &model.RPCError{Code: taskCheckUnsupported, Outcome: outcome, Cause: err,
		Message: "preflight not supported by this node: it predates tasks.check; upgrade it to check task specifications. A check never submits work (" + message + ")"}
}
