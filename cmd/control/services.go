package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/logfile"
	"github.com/koltyakov/control/internal/node"
)

// gatewayCLI runs the gateway service. commandArgs are the original process
// arguments, reused when a managed update restarts the service.
func gatewayCLI(ctx context.Context, args, commandArgs []string, token string) error {
	f := flag.NewFlagSet("gateway", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:7330", "listen address")
	data := f.String("data", ".control-gateway", "gateway state directory")
	cert := f.String("tls-cert", "", "TLS certificate PEM")
	key := f.String("tls-key", "", "TLS private key PEM")
	limits := gateway.DefaultLimits()
	f.IntVar(&limits.Connections, "max-connections", limits.Connections, "maximum gateway connections, including handshakes")
	f.IntVar(&limits.ConnectionsPerAccount, "max-connections-per-account", limits.ConnectionsPerAccount, "maximum connections per account")
	f.IntVar(&limits.ClientsPerAccount, "max-clients-per-account", limits.ClientsPerAccount, "maximum client connections per account")
	f.IntVar(&limits.NodesPerAccount, "max-nodes-per-account", limits.NodesPerAccount, "maximum connected nodes per account")
	f.IntVar(&limits.Handshakes, "max-handshakes", limits.Handshakes, "maximum concurrent gateway handshakes")
	f.IntVar(&limits.HandshakesPerAccount, "max-handshakes-per-account", limits.HandshakesPerAccount, "maximum concurrent handshakes per account")
	f.IntVar(&limits.NewIdentitiesPerMinute, "new-identities-per-minute", limits.NewIdentitiesPerMinute, "new identity registrations per minute per account, with an equal burst")
	f.Int64Var(&limits.QueuedBytes, "max-queued-bytes", limits.QueuedBytes, "global retained packet byte budget")
	f.Int64Var(&limits.QueuedBytesPerAccount, "max-queued-bytes-per-account", limits.QueuedBytesPerAccount, "retained packet byte budget per account")
	if err := f.Parse(args); err != nil {
		return err
	}
	if err := limits.Validate(); err != nil {
		return err
	}
	return managed(ctx, *data, commandArgs, func(ctx context.Context, apply func(string) error) error {
		interval := 15 * time.Minute
		if value := os.Getenv("CONTROL_RELEASE_INTERVAL"); value != "" {
			var err error
			interval, err = time.ParseDuration(value)
			if err != nil || interval < time.Second {
				return errors.New("invalid release interval")
			}
		}
		var stunServers []string
		if value := os.Getenv("CONTROL_STUN_SERVERS"); value != "" {
			for server := range strings.SplitSeq(value, ",") {
				stunServers = append(stunServers, strings.TrimSpace(server))
			}
		}
		g, err := gateway.New(*data, token, gateway.Options{Limits: limits, STUNServers: stunServers, PublicURL: os.Getenv("CONTROL_PUBLIC_URL"), SuperuserKey: os.Getenv("CONTROL_SUPERUSER_KEY"), Software: buildinfo.Current(), Apply: apply, ReleaseRepo: releaseRepository(), ReleaseToken: os.Getenv("CONTROL_RELEASE_TOKEN"), ReleaseInterval: interval})
		if err != nil {
			return err
		}
		defer func() { _ = g.Close() }()
		g.StartUpdates(ctx)
		return serve(ctx, node.HTTPServer(*listen, g.Handler()), *cert, *key)
	})
}

// nodeCLI runs an enrolled execution node from its configuration file.
// Gateway administration credentials are removed from the environment so
// providers and subprocesses cannot inherit them.
func nodeCLI(ctx context.Context, args, commandArgs []string, token string) error {
	_ = os.Unsetenv("CONTROL_USER_KEY")
	_ = os.Unsetenv("CONTROL_SUPERUSER_KEY")
	_ = os.Unsetenv("CONTROL_RELEASE_TOKEN")
	f := flag.NewFlagSet("node", flag.ContinueOnError)
	path := f.String("config", "control.json", "node configuration JSON")
	if err := f.Parse(args); err != nil {
		return err
	}
	cfg, err := node.LoadConfig(*path)
	if err != nil {
		return err
	}
	if token != "" {
		cfg.Token = token
	}
	ctx = withManagedNodeProfile(ctx, *path)
	return managed(ctx, cfg.DataDir, commandArgs, func(ctx context.Context, apply func(string) error) (err error) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		n, err := node.New(cfg)
		if err != nil {
			return err
		}
		defer func() {
			_ = n.Close()
			if n.Unregistered() {
				if uninstallErr := installation.ScheduleRetiredUninstall(*path, n.Identity.ID); uninstallErr != nil {
					err = errors.Join(err, uninstallErr)
					slog.Error("uninstall retired node", "error", uninstallErr)
				}
			}
		}()
		logs := make(chan struct{})
		go func() { defer close(logs); logfile.Maintain(ctx, filepath.Join(filepath.Dir(*path), "node.log")) }()
		defer func() { cancel(); <-logs }()
		n.SetShutdown(cancel)
		n.ConfigureStartup(func(ctx context.Context, mode string) error {
			return installation.PrepareStartupChange(ctx, *path, n.Identity.ID, mode)
		})
		if err = n.ConfigureUpdates(buildinfo.Current(), apply); err != nil {
			return err
		}
		if err = n.Start(ctx); err != nil {
			if errors.Is(err, node.ErrUnregistered) {
				slog.Info("machine unregistered; stopping service")
				return nil
			}
			return err
		}
		slog.Info("node online", "name", n.Config.Name, "id", n.Identity.ID, "api", n.Config.Listen)
		server := node.HTTPServer(n.Config.Listen, n.Handler())
		// Long polls, downloads, and log follows end with the service, so
		// graceful shutdown does not wait for them to time out.
		server.BaseContext = func(net.Listener) context.Context { return ctx }
		return serve(ctx, server, "", "")
	})
}
