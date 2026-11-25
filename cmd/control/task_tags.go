package main

import (
	"context"
	"errors"
	"flag"
	"fmt"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

// taskPageParams sends tag only when set, so untagged listings stay unchanged
// for older workers and need no support check.
func taskPageParams(limit int, cursor, tag string) map[string]any {
	params := map[string]any{"limit": limit, "cursor": cursor}
	if tag != "" {
		params["tag"] = tag
	}
	return params
}

// taskCancelTaggedCLI cancels the caller's own unfinished tasks carrying one
// tag on one node and prints every outcome.
func taskCancelTaggedCLI(ctx context.Context, c client.Client, target string, args []string) error {
	f := flag.NewFlagSet("task cancel", flag.ContinueOnError)
	tag := f.String("tag", "", "cancel your unfinished tasks on NODE carrying this tag")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *tag == "" {
		return errors.New("usage: control task cancel NODE --tag TAG, or control task cancel NODE ID [--spec]")
	}
	if err := model.ValidateTaskTag(*tag); err != nil {
		return err
	}
	var result model.TaggedCancellation
	if err := c.Call(ctx, target, model.CancelTaggedTasks, map[string]any{"tag": *tag}, &result); err != nil {
		return err
	}
	printJSON(result)
	failed := 0
	for _, task := range result.Tasks {
		if task.Outcome != model.TaggedCancelRequested {
			failed++
		}
	}
	if failed != 0 {
		return fmt.Errorf("%d of %d tagged task cancellations failed; inspect them by ID", failed, len(result.Tasks))
	}
	return nil
}
