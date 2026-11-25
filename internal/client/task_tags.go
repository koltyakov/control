package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/koltyakov/control/internal/model"
)

// requireTaskTags refuses, before anything is sent, a request that carries
// task tags or a tag filter for a node that does not advertise task tags.
// Older nodes ignore the unknown fields, so they would accept the task
// untagged, where a later cancel-by-tag silently misses it, or list every
// task as if it matched. Requests without tags cost nothing extra.
func (c Client) requireTaskTags(ctx context.Context, target, method string, params any) error {
	switch method {
	case "tasks.start", "tasks.page", "tasks.list", "peers.call", "workflow.run":
	default:
		return nil
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	for _, destination := range model.TaskTagDestinations(target, method, raw) {
		var description json.RawMessage
		if err := c.call(ctx, destination, "node.describe", map[string]any{}, &description); err != nil {
			return fmt.Errorf("check task tag support on %s; nothing was submitted: %w", describeTarget(destination), err)
		}
		if !model.TaskTagsAdvertised(description) {
			return model.OperationError(fmt.Errorf("%s does not advertise task tags; upgrade it before sending tags or tag filters; nothing was submitted", describeTarget(destination)), "not_sent")
		}
	}
	return nil
}

func describeTarget(target string) string {
	if target == "" {
		return "the local node"
	}
	return fmt.Sprintf("node %q", target)
}

// withTaskTags keeps a task's tags visible after its specification is omitted
// from compact CLI and MCP results. Original is the record before compaction.
func withTaskTags(compact, original json.RawMessage) json.RawMessage {
	var record struct {
		Spec struct {
			Tags []string `json:"tags"`
		} `json:"spec"`
	}
	if json.Unmarshal(original, &record) != nil || len(record.Spec.Tags) == 0 || len(compact) < 2 || compact[len(compact)-1] != '}' {
		return compact
	}
	out := append([]byte{}, compact[:len(compact)-1]...)
	if len(out) > 1 {
		out = append(out, ',')
	}
	out = append(out, `"tags":`...)
	out = append(out, model.JSON(record.Spec.Tags)...)
	return append(out, '}')
}

// taskTagsCheckedKey marks a request whose tag support StartTask already
// checked, so the shared call path does not describe the target again.
type taskTagsCheckedKey struct{}

func (c Client) requireTaskTagsOnce(ctx context.Context, target, method string, params any) error {
	if checked, _ := ctx.Value(taskTagsCheckedKey{}).(bool); checked {
		return nil
	}
	return c.requireTaskTags(ctx, target, method, params)
}
