package node

import (
	"context"
	"errors"
	"sort"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) taskPage(ctx context.Context, owner, cursor string, limit int) (model.TaskPage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 || (cursor != "" && !safeID.MatchString(cursor)) {
		return model.TaskPage{}, errors.New("task page requires limit 1..200 and a task-ID cursor")
	}
	// Keep only the first limit+1 IDs. Memory is bounded even with extended retention.
	n.mu.Lock()
	ids := make([]string, 0, limit+1)
	for id, task := range n.tasks {
		if id <= cursor || (task.Owner != owner && owner != n.Identity.ID) {
			continue
		}
		i := sort.SearchStrings(ids, id)
		if i > limit {
			continue
		}
		ids = append(ids, "")
		copy(ids[i+1:], ids[i:])
		ids[i] = id
		if len(ids) > limit+1 {
			ids = ids[:limit+1]
		}
	}
	page := model.TaskPage{Tasks: make([]model.TaskSummary, 0, min(len(ids), limit))}
	if len(ids) > limit {
		page.NextCursor = ids[limit-1]
		ids = ids[:limit]
	}
	for _, id := range ids {
		t := n.tasks[id]
		summary := model.TaskSummary{ID: id, Capability: t.Spec.Capability, State: t.State, Phase: t.Phase, Created: t.Created, Updated: t.Updated, Error: t.Error}
		if len(summary.Error) > 1024 {
			summary.Error = summary.Error[:1024]
			summary.ErrorTruncated = true
		}
		page.Tasks = append(page.Tasks, summary)
	}
	n.mu.Unlock()
	return page, ctx.Err()
}
