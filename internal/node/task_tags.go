package node

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/koltyakov/control/internal/model"
)

// cancelTagged signals cancellation of the caller's own unfinished tasks that
// carry tag, as tasks.cancel does for each. It acts on the set observed under
// n.mu: an acceptance still being written is not yet in n.tasks and is left
// for a later call. Ownership is exact, including for the node's own local API
// identity, which can otherwise inspect every task. Nothing is deleted,
// persisted or replayed, so repeating the call is harmless.
//
// Delegated grants are bound to one task ID, so a grant never reaches this
// method: authorize rejects it and access.grant refuses to issue it. This
// check keeps the rule local even if those lists change.
func (n *Node) cancelTagged(ctx context.Context, owner string, args json.RawMessage) (model.TaggedCancellation, error) {
	if a, ok := ctx.Value(authorityContextKey{}).(authority); ok && a.grant != nil {
		return model.TaggedCancellation{}, errors.New("tasks.cancelTagged is not available under a delegated grant; cancel delegated tasks by ID")
	}
	var q struct {
		Tag string `json:"tag"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return model.TaggedCancellation{}, err
	}
	if err := model.ValidateTaskTag(q.Tag); err != nil {
		return model.TaggedCancellation{}, err
	}
	result := model.TaggedCancellation{Tag: q.Tag, Tasks: []model.TaggedCancelResult{}}
	// One pass over retained tasks, as tasks.page does. Unfinished tasks are
	// bounded by the 256-task queue limit, so the outcome list is bounded too.
	n.mu.Lock()
	for id, task := range n.tasks {
		if task.Owner != owner || !slices.Contains(task.Spec.Tags, q.Tag) {
			continue
		}
		if task.Terminal() {
			result.AlreadyTerminal++
			continue
		}
		outcome := model.TaggedCancelResult{ID: id, Outcome: model.TaggedCancelRequested, State: task.State}
		if cancel := n.cancels[id]; cancel != nil {
			cancel()
		} else {
			outcome.Outcome, outcome.Error = model.TaggedCancelFailed, "task has no active cancellation handle; inspect it with tasks.get"
		}
		result.Tasks = append(result.Tasks, outcome)
	}
	n.mu.Unlock()
	slices.SortFunc(result.Tasks, func(a, b model.TaggedCancelResult) int { return strings.Compare(a.ID, b.ID) })
	return result, nil
}
