package node

import (
	"context"
	"errors"
	"slices"
	"sort"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) taskPage(ctx context.Context, owner, cursor string, limit int) (model.TaskPage, error) {
	return n.taggedTaskPage(ctx, owner, cursor, "", limit)
}

// taggedTaskPage filters by tag during the same bounded scan: each retained
// task costs at most MaxTaskTags comparisons, and no tag index is kept.
func (n *Node) taggedTaskPage(ctx context.Context, owner, cursor, tag string, limit int) (model.TaskPage, error) {
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 200 || (cursor != "" && !safeID.MatchString(cursor)) {
		return model.TaskPage{}, errors.New("task page requires limit 1..200 and a task-ID cursor")
	}
	if tag != "" {
		if err := model.ValidateTaskTag(tag); err != nil {
			return model.TaskPage{}, err
		}
	}
	// Keep only the first limit+1 IDs. Memory is bounded even with extended retention.
	n.mu.Lock()
	ids := make([]string, 0, limit+1)
	for id, task := range n.tasks {
		if id <= cursor || (task.Owner != owner && owner != n.Identity.ID) || (tag != "" && !slices.Contains(task.Spec.Tags, tag)) {
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
		summary := model.SummarizeTask(*n.tasks[id])
		summary.Tags = slices.Clone(summary.Tags)
		page.Tasks = append(page.Tasks, summary)
	}
	n.mu.Unlock()
	return page, ctx.Err()
}
