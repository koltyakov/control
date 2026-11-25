package model

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
)

// Task tags are owner metadata for listing and bulk cancellation. They grant
// no authority and select nothing by themselves.
const (
	MaxTaskTags       = 8
	MaxTaskTagLength  = 64
	TaskTagsProtocol  = "task-tags-v1"
	CancelTaggedTasks = "tasks.cancelTagged"
)

// TaskTagPattern is a conservative portable alphabet: an ASCII letter or digit
// followed by up to 63 letters, digits, '.', '_' or '-'. Tags are case-sensitive.
var TaskTagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateTaskTag checks one tag, as used by filters and cancel-by-tag.
func ValidateTaskTag(tag string) error {
	if !TaskTagPattern.MatchString(tag) {
		return fmt.Errorf("invalid task tag %q: use 1-%d letters, digits, '.', '_' or '-', starting with a letter or digit", tag, MaxTaskTagLength)
	}
	return nil
}

// NormalizeTaskTags validates, sorts and de-duplicates tags so comparison and
// the idempotency digest are order-insensitive. No tags normalize to nil, which
// keeps the encoding, and so the digest, of an untagged specification unchanged.
func NormalizeTaskTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	if len(tags) > MaxTaskTags {
		return nil, fmt.Errorf("task specification allows at most %d tags", MaxTaskTags)
	}
	for _, tag := range tags {
		if err := ValidateTaskTag(tag); err != nil {
			return nil, err
		}
	}
	normalized := slices.Clone(tags)
	slices.Sort(normalized)
	return slices.Compact(normalized), nil
}

// TaskTagsDescription is advertised by node.describe. Clients refuse to send
// tags or tag filters to a node without it, because older nodes ignore the
// unknown fields and would accept tasks untagged or list unfiltered tasks.
func TaskTagsDescription() map[string]any {
	return map[string]any{"protocol": TaskTagsProtocol, "maxTags": MaxTaskTags, "maxLength": MaxTaskTagLength, "pattern": TaskTagPattern.String(), "filters": []string{"tasks.page", "tasks.list"}, "methods": []string{CancelTaggedTasks}}
}

// TaskTagsAdvertised reports whether a node.describe result supports tags.
func TaskTagsAdvertised(description json.RawMessage) bool {
	var d struct {
		TaskTags struct {
			Protocol string `json:"protocol"`
		} `json:"taskTags"`
	}
	return json.Unmarshal(description, &d) == nil && d.TaskTags.Protocol == TaskTagsProtocol
}

// TaggedCancellation reports cancel-by-tag. Tasks lists every matching task
// that was unfinished when the node observed the set; AlreadyTerminal counts
// matching retained tasks that had already finished and were left unchanged.
type TaggedCancellation struct {
	Tag             string               `json:"tag"`
	Tasks           []TaggedCancelResult `json:"tasks"`
	AlreadyTerminal int                  `json:"alreadyTerminal"`
}

// Tagged cancellation outcomes.
const (
	// TaggedCancelRequested means cancellation was signalled exactly as by
	// tasks.cancel. The task becomes terminal asynchronously and may still
	// end succeeded or failed if it was already finishing.
	TaggedCancelRequested = "cancelRequested"
	TaggedCancelFailed    = "failed"
)

type TaggedCancelResult struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	// State is the task state observed when cancellation was signalled.
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}

// TaskTagDestinations lists the nodes a request depends on for tag support:
// destinations of tagged task specifications, including forwarded tasks and
// workflow steps, and targets of tag filters. Target is the node addressed by
// the request itself. An older node fails tasks.cancelTagged explicitly as an
// unknown method, so it needs no check. Undecodable requests list nothing and
// are left to the normal validation path.
func TaskTagDestinations(target, method string, params json.RawMessage) []string {
	var destinations []string
	var inspect func(target, method string, params json.RawMessage, depth int)
	inspect = func(target, method string, params json.RawMessage, depth int) {
		if depth > 2 {
			return
		}
		add := func() {
			if !slices.Contains(destinations, target) {
				destinations = append(destinations, target)
			}
		}
		switch method {
		case "tasks.page", "tasks.list":
			var q struct {
				Tag string `json:"tag"`
			}
			if json.Unmarshal(params, &q) == nil && q.Tag != "" {
				add()
			}
		case "tasks.start":
			var spec TaskSpec
			if json.Unmarshal(params, &spec) != nil {
				return
			}
			if len(spec.Tags) != 0 {
				add()
			}
			if spec.Capability == "peers.call" || spec.Capability == "workflow.run" {
				inspect(target, spec.Capability, spec.Args, depth+1)
			}
		case "peers.call":
			var q struct {
				Target string          `json:"target"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(params, &q) == nil && q.Method == "tasks.start" {
				inspect(q.Target, q.Method, q.Params, depth+1)
			}
		case "workflow.run":
			var plan WorkflowPlan
			if json.Unmarshal(params, &plan) != nil {
				return
			}
			for _, step := range plan.Steps {
				if len(step.Task.Tags) != 0 && !slices.Contains(destinations, step.Target) {
					destinations = append(destinations, step.Target)
				}
			}
		}
	}
	inspect(target, method, params, 0)
	return destinations
}
