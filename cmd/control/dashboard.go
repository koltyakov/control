package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/dashboard"
	"github.com/koltyakov/control/internal/model"
	"golang.org/x/term"
)

func dashboardCLI(ctx context.Context, c client.Client, args []string) error {
	f := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	once := f.Bool("once", false, "print a single snapshot")
	jsonOutput := f.Bool("json", false, "print one JSON snapshot")
	interval := f.Duration("interval", 2*time.Second, "activity refresh interval; system sampling is separate")
	timeout := f.Duration("timeout", 15*time.Second, "snapshot request timeout")
	nodes := f.String("node", "", "comma-separated node names or IDs")
	recent := f.Int("recent", 5, "recent completions per node, 0..64")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control dashboard [--once] [--json] [--node NAME,...] [--interval 2s] [--recent 5]")
	}
	if *interval < 250*time.Millisecond || *timeout <= 0 || *recent < 0 || *recent > 64 {
		return errors.New("interval must be at least 250ms, timeout positive, and recent 0..64")
	}
	query := model.PoolActivityQuery{Recent: *recent}
	if *nodes != "" {
		for _, name := range strings.Split(*nodes, ",") {
			if name = strings.TrimSpace(name); name != "" {
				query.Nodes = append(query.Nodes, name)
			}
		}
	}
	fetch := func(ctx context.Context) (model.PoolActivitySnapshot, error) { return c.Activities(ctx, query) }
	if *once || *jsonOutput || !term.IsTerminal(int(os.Stdout.Fd())) || !term.IsTerminal(int(os.Stdin.Fd())) {
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		snapshot, err := fetch(ctx)
		if err != nil {
			return err
		}
		if *jsonOutput {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			return encoder.Encode(snapshot)
		}
		_, err = fmt.Fprintln(os.Stdout, dashboard.Render(snapshot, time.Now()))
		return err
	}
	return dashboard.Run(ctx, fetch, dashboard.Options{Interval: *interval, Timeout: *timeout, Output: os.Stdout})
}
