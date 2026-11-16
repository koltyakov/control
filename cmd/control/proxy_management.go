package main

import (
	"context"
	"errors"
	"flag"
	"strings"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/model"
)

func machineProxySettings(ctx context.Context, c client.Admin, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control machines proxies inspect|enable|configure|disable NAME [--interface NIC --address IP --port PORT --sources CIDR[,CIDR]]")
	}
	action := args[0]
	if action != "inspect" && action != "enable" && action != "configure" && action != "disable" {
		return errors.New("expected proxies inspect, enable, configure, disable, or release")
	}
	q := model.ProxyConfigure{Enabled: action == "enable" || action == "configure"}
	f := flag.NewFlagSet("machines proxies "+action, flag.ContinueOnError)
	f.StringVar(&q.Interface, "interface", "", "native interface name")
	f.StringVar(&q.Address, "address", "", "private address assigned to the selected interface")
	f.IntVar(&q.Port, "port", 8080, "listener port")
	sources := f.String("sources", "", "explicit comma-separated private source CIDRs")
	if err := f.Parse(args[2:]); err != nil {
		return err
	}
	if f.NArg() != 0 || (!q.Enabled && f.NFlag() != 0) {
		return errors.New("unexpected proxy settings arguments")
	}
	if q.Enabled {
		for _, source := range strings.Split(*sources, ",") {
			q.Sources = append(q.Sources, strings.TrimSpace(source))
		}
		if _, err := q.Endpoint(); err != nil {
			return err
		}
	}
	n, err := c.ResolveMachine(ctx, args[1])
	if err != nil {
		return err
	}
	var result model.ProxyConfiguration
	if action == "inspect" {
		result, err = c.ProxyConfiguration(ctx, n.ID)
	} else {
		if !q.Enabled {
			q = model.ProxyConfigure{}
		}
		result, err = c.ConfigureProxy(ctx, n.ID, q)
	}
	if err != nil {
		return err
	}
	printJSON(result)
	return nil
}
