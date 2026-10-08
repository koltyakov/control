package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func speedtestCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) == 0 || args[0] == "" {
		return errors.New("usage: control speedtest NODE [--from WORKER] [--size MiB] [--samples N] [--timeout DURATION] [--json]")
	}
	f := flag.NewFlagSet("speedtest", flag.ContinueOnError)
	from := f.String("from", "", "source worker; omitted tests from this orchestrator")
	size := f.Int64("size", 16, "MiB transferred in each direction, 1..256")
	samples := f.Int("samples", 10, "round-trip samples, 1..100")
	timeout := f.Duration("timeout", model.ConnectionTestTimeout, "overall deadline, at most 2m")
	jsonOutput := f.Bool("json", false, "print structured results")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("unexpected speedtest arguments")
	}
	if *size < 1 || *size > 256 {
		return errors.New("speedtest size must be 1..256 MiB per direction")
	}
	if *samples < 1 || *samples > 100 {
		return errors.New("speedtest samples must be 1..100")
	}
	if *timeout < time.Second || *timeout > model.ConnectionTestTimeout {
		return errors.New("speedtest timeout must be between 1s and 2m")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, err := c.TestConnection(ctx, client.ConnectionTestSpec{Node: args[0], From: *from, ConnectionTestOptions: model.ConnectionTestOptions{Bytes: *size << 20, Samples: *samples}})
	if err != nil {
		return err
	}
	if *jsonOutput {
		printJSON(result)
		return nil
	}
	fmt.Printf("%s <-> %s (%s)\n", result.Source, result.Target, result.Transport)
	fmt.Printf("Setup %.1f ms\n", result.SetupMillis)
	fmt.Printf("RTT %.2f ms average, %.2f min, %.2f max; jitter %.2f ms (%d samples)\n", result.Latency.MeanMillis, result.Latency.MinMillis, result.Latency.MaxMillis, result.Latency.JitterMillis, result.Latency.Samples)
	fmt.Printf("%s -> %s: %.2f Mbps (%d bytes, %.2fs)\n", result.Source, result.Target, result.Upload.Mbps, result.Upload.Bytes, result.Upload.Seconds)
	fmt.Printf("%s -> %s: %.2f Mbps (%d bytes, %.2fs)\n", result.Target, result.Source, result.Download.Mbps, result.Download.Bytes, result.Download.Seconds)
	return nil
}
