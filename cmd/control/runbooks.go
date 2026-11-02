package main

import (
	"context"
	"errors"
	"flag"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/runbooks"
)

func parseRunbookQuery(args []string) (runbooks.Query, error) {
	var q runbooks.Query
	f := flag.NewFlagSet("runbooks", flag.ContinueOnError)
	f.StringVar(&q.Node, "node", "", "machine alias or ID; omitted lists local shared and machine-specific runbooks")
	f.StringVar(&q.Source, "source", "", "exact origin: project, global, worker-user, worker-common")
	f.StringVar(&q.File, "file", "", "complete relative document path from discovery; requires source")
	f.BoolVar(&q.Local, "local", false, "read only project/global stores, without contacting a worker")
	if err := f.Parse(args); err != nil {
		return q, err
	}
	if f.NArg() != 0 {
		return q, errors.New("usage: control runbooks [--node NAME] [--local] [--source SOURCE --file FILE]")
	}
	return q, q.Validate()
}

func runbooksCLI(ctx context.Context, c client.Client, args []string) error {
	q, err := parseRunbookQuery(args)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	catalog, err := c.Runbooks(ctx, q)
	if err != nil {
		return err
	}
	printJSON(catalog)
	return nil
}
