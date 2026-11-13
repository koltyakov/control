package node

import (
	"context"
	"encoding/json"

	"github.com/koltyakov/control/internal/model"
)

// Each dispatch owns its receipt. Workflow children and concurrent local calls
// get separate scopes, and accepted tasks do not inherit this mutable holder.
type submissionReceiptKey struct{}
type submissionReceipt struct {
	value *model.TaskSubmissionReceipt
}

func recordSubmission(ctx context.Context, id, status string) {
	if scope, ok := ctx.Value(submissionReceiptKey{}).(*submissionReceipt); ok {
		scope.value = &model.TaskSubmissionReceipt{TaskID: id, Status: status}
	}
}

func (n *Node) dispatchWithReceipt(ctx context.Context, caller, method string, args json.RawMessage) (any, *model.TaskSubmissionReceipt, error) {
	scope := &submissionReceipt{}
	ctx = context.WithValue(ctx, submissionReceiptKey{}, scope)
	value, err := n.dispatch(ctx, caller, method, args)
	return value, scope.value, err
}
