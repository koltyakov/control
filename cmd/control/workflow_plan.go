package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

// resolveTaskPlan resolves workflow select/each targets before sending and
// prints every concrete step, like the task ID, so a lost reply can be
// reconciled against the exact machines and child IDs.
func resolveTaskPlan(ctx context.Context, c client.Client, target string, spec model.TaskSpec) (model.TaskSpec, error) {
	resolved, steps, err := c.ResolveTask(ctx, target, spec)
	if err != nil {
		return spec, fmt.Errorf("resolve workflow targets for task %s: %w", spec.ID, err)
	}
	printResolvedSteps(os.Stderr, steps)
	return resolved, nil
}

func printResolvedSteps(w io.Writer, steps []client.ResolvedStep) {
	for _, step := range steps {
		machine := step.Target
		if step.Node != "" {
			machine = step.Node + " (" + step.Target + ")"
		}
		_, _ = fmt.Fprintf(w, "step %s: target %s task %s\n", step.Name, machine, step.TaskID)
	}
}
