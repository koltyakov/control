package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	args := os.Args[1:]
	if len(args) == 2 && args[0] == "__tunnels" {
		logFile, err := os.OpenFile(filepath.Join(args[1], "service.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			defer func() { _ = logFile.Close() }()
			os.Stderr = logFile
		}
		if err = client.RunTunnelService(ctx, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, "control tunnels:", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "__uninstall" {
		if err := retiredUninstallCLI(ctx, args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, "control uninstall:", err)
			os.Exit(1)
		}
		return
	}
	if handled, err := runPlatformService(ctx, args); handled {
		if err != nil {
			fmt.Fprintln(os.Stderr, "control:", err)
			os.Exit(1)
		}
		return
	}
	if len(args) > 0 && args[0] == "__managed" {
		managedChild = true
		args = args[1:]
		go func() { _, _ = io.Copy(io.Discard, os.Stdin); cancel() }()
	}
	if err := run(ctx, args); err != nil {
		fmt.Fprintln(os.Stderr, "control:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	commandArgs := append([]string(nil), args...)
	flags := flag.NewFlagSet("control", flag.ContinueOnError)
	profile := flags.String("config", installation.ConfigPath(), "saved local node configuration for CLI/MCP")
	api := flags.String("api", env("CONTROL_API", "http://127.0.0.1:7331"), "local node API URL")
	token := flags.String("token", os.Getenv("CONTROL_TOKEN"), "gateway/local API token")
	debug := flags.Bool("debug", false, "debug logs to stderr")
	admin := func() client.Admin {
		c := adminClient(*profile)
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "token" {
				c.Key = *token
			}
		})
		return c
	}
	flags.Usage = func() { fmt.Print(commandHelp(ctx, admin())) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		fmt.Print(commandHelp(ctx, admin()))
		return nil
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	c := (client.Client{URL: *api, Token: *token}).WithLifetime(ctx)
	defer func() { _ = c.Close() }()
	if args[0] != "node" && args[0] != "gateway" && args[0] != "version" && args[0] != "update" && args[0] != "upgrade" && args[0] != "install-self" {
		cfg, err := installation.ReadConfig(*profile)
		if err != nil {
			return err
		}
		explicitAPI, explicitToken := false, false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "api" {
				explicitAPI = true
			}
			if f.Name == "token" {
				explicitToken = true
			}
		})
		if !explicitAPI && os.Getenv("CONTROL_API") == "" && cfg.Name != "" {
			c.URL = installation.API(cfg)
		}
		if !explicitToken && os.Getenv("CONTROL_TOKEN") == "" {
			c.Token = cfg.Token
		}
		switch args[0] {
		case "mcp", "system", "call", "exec", "task", "artifact", "tunnel", "session", "clipboard", "dashboard", "speedtest":
			if !explicitAPI && os.Getenv("CONTROL_API") == "" {
				var override *string
				if explicitToken {
					override = token
				}
				remote, err := gatewayClient(*profile, override)
				if err != nil {
					return err
				}
				c = c.WithStandalone(ctx, client.StandaloneConfig{Gateway: remote,
					StateDir: filepath.Join(installation.Home(), "clients"), RelayOnly: cfg.RelayOnly, ICEServers: cfg.ICEServers, AccountRouting: true})
			}
		}
	}
	c = c.WithPersistentTunnels(filepath.Join(installation.Home(), "tunnels"), func(ctx context.Context, dir string) error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		return installation.StartTunnelService(ctx, binary, dir)
	})
	switch args[0] {
	case "login":
		return loginCLI(ctx, args[1:])
	case "secrets":
		return secretsCLI(ctx, args[1:], *profile)
	case "install-self":
		path, err := installation.InstallSelf(ctx)
		if err != nil {
			return err
		}
		fmt.Println("Installed", path)
		return installAgent(ctx, "opencode", "", path, *profile, false, true)
	case "setup":
		return setupCLI(ctx, args[1:], *profile)
	case "enroll":
		return enrollCLI(ctx, args[1:], *profile)
	case "service":
		return serviceCLI(ctx, args[1:], *profile)
	case "install-mcp", "install-skill":
		return agentInstallCLI(ctx, args[0], args[1:], *profile)
	case "help", "--help", "-h":
		fmt.Print(commandHelp(ctx, admin()))
		return nil
	case "version":
		if len(args) > 1 && args[1] == "--json" {
			printJSON(buildinfo.Current())
		} else {
			fmt.Println("control", buildinfo.Version)
			if buildinfo.BuildTime != "" {
				fmt.Println("built", buildinfo.BuildTime)
			}
		}
		return nil
	case "update":
		if len(args) > 1 && (args[1] == "push" || args[1] == "status" || args[1] == "check" || args[1] == "authorize") {
			var override *string
			flags.Visit(func(f *flag.Flag) {
				if f.Name == "token" {
					override = token
				}
			})
			remote, err := updateAdminClient(*profile, override)
			if err != nil {
				return err
			}
			if args[1] == "authorize" {
				return authorizeUpdateCLI(ctx, remote, args[2:], override == nil && os.Getenv("CONTROL_SUPERUSER_KEY") == "")
			}
			return adminCLI(ctx, remote, args)
		}
		return selfUpdateCLI(ctx, args[1:])
	case "upgrade":
		return selfUpdateCLI(ctx, args[1:])
	case "keys", "users":
		return adminCLI(ctx, admin(), args)
	case "gateway":
		f := flag.NewFlagSet("gateway", flag.ContinueOnError)
		listen := f.String("listen", "127.0.0.1:7330", "listen address")
		data := f.String("data", ".control-gateway", "gateway state directory")
		cert := f.String("tls-cert", "", "TLS certificate PEM")
		key := f.String("tls-key", "", "TLS private key PEM")
		if err := f.Parse(args[1:]); err != nil {
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
			g, err := gateway.New(*data, *token, gateway.Options{PublicURL: os.Getenv("CONTROL_PUBLIC_URL"), SuperuserKey: os.Getenv("CONTROL_SUPERUSER_KEY"), Software: buildinfo.Current(), Apply: apply, ReleaseRepo: releaseRepository(), ReleaseToken: os.Getenv("CONTROL_RELEASE_TOKEN"), ReleaseInterval: interval})
			if err != nil {
				return err
			}
			defer func() { _ = g.Close() }()
			g.StartUpdates(ctx)
			return serve(ctx, node.HTTPServer(*listen, g.Handler()), *cert, *key)
		})
	case "node":
		_ = os.Unsetenv("CONTROL_USER_KEY")
		_ = os.Unsetenv("CONTROL_SUPERUSER_KEY")
		_ = os.Unsetenv("CONTROL_RELEASE_TOKEN")
		f := flag.NewFlagSet("node", flag.ContinueOnError)
		path := f.String("config", "control.json", "node configuration JSON")
		if err := f.Parse(args[1:]); err != nil {
			return err
		}
		cfg, err := node.LoadConfig(*path)
		if err != nil {
			return err
		}
		if *token != "" {
			cfg.Token = *token
		}
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
			n.SetShutdown(cancel)
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
			return serve(ctx, node.HTTPServer(n.Config.Listen, n.Handler()), "", "")
		})
	case "mcp":
		return c.MCPServer().Run(ctx, &mcp.StdioTransport{})
	case "machines":
		if len(args) > 1 {
			return machineCLI(ctx, admin(), args[1:])
		}
		var machines []model.Node
		if err := admin().JSON(ctx, http.MethodGet, "/v1/nodes", nil, &machines); err != nil {
			return err
		}
		printJSON(machines)
		return nil
	case "dashboard":
		var override *string
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "token" {
				override = token
			}
		})
		remote, err := gatewayClient(*profile, override)
		if err != nil {
			return err
		}
		return dashboardCLI(ctx, c, remote, args[1:])
	case "system":
		if len(args) < 2 {
			return errors.New("usage: control system NODE [--refresh]")
		}
		f := flag.NewFlagSet("system", flag.ContinueOnError)
		refresh := f.Bool("refresh", false, "take a new resource sample instead of returning the cache")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected system arguments")
		}
		return callPrint(ctx, c, args[1], "system.info", map[string]any{"refresh": *refresh})
	case "call":
		if len(args) < 3 {
			return errors.New("usage: control call NODE METHOD [JSON|@file]")
		}
		params := json.RawMessage(`{}`)
		var err error
		if len(args) > 3 {
			params, err = readJSON(args[3])
			if err != nil {
				return err
			}
		}
		return callPrint(ctx, c, args[1], args[2], params)
	case "exec":
		return execCLI(ctx, c, args[1:])
	case "session":
		if len(args) != 1 {
			return errors.New("usage: control session")
		}
		info, err := c.Session(ctx)
		if err != nil {
			return err
		}
		printJSON(info)
		return nil
	case "speedtest":
		return speedtestCLI(ctx, c, args[1:])
	case "clipboard":
		return clipboardCLI(ctx, c, args[1:])
	case "task":
		if len(args) < 3 {
			return errors.New("usage: control task start|get|wait|cancel|logs|list NODE [ID|@spec.json]")
		}
		method, target := args[1], args[2]
		if method == "list" {
			return callPrint(ctx, c, target, "tasks.list", map[string]any{})
		}
		if len(args) < 4 {
			return errors.New("task ID or specification is required")
		}
		if method == "start" {
			b, err := readJSON(args[3])
			if err != nil {
				return err
			}
			var spec model.TaskSpec
			if err = json.Unmarshal(b, &spec); err != nil {
				return err
			}
			return submit(ctx, c, target, spec, false)
		}
		if method == "wait" {
			task, err := c.Wait(ctx, target, args[3])
			if err != nil {
				return err
			}
			printJSON(task)
			if task.State != "succeeded" {
				return errors.New(task.Error)
			}
			return nil
		}
		if method == "logs" {
			return logsCLI(ctx, c, target, args[3:])
		}
		switch method {
		case "get", "cancel", "logs":
		default:
			return errors.New("unknown task command")
		}
		return callPrint(ctx, c, target, "tasks."+method, map[string]any{"id": args[3]})
	case "artifact":
		return artifactCLI(ctx, c, args[1:])
	case "tunnel":
		return tunnelCLI(ctx, c, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func submit(ctx context.Context, c client.Client, target string, spec model.TaskSpec, wait bool) error {
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	fmt.Fprintln(os.Stderr, "task", spec.ID)
	var task model.Task
	if err := c.Call(ctx, target, "tasks.start", spec, &task); err != nil {
		return fmt.Errorf("submit task %s; query this ID before retrying: %w", spec.ID, err)
	}
	if wait {
		var err error
		task, err = c.Wait(ctx, target, task.ID)
		if err != nil {
			return err
		}
	}
	printJSON(task)
	if task.Terminal() && task.State != "succeeded" {
		return errors.New(task.Error)
	}
	return nil
}

func artifactCLI(ctx context.Context, c client.Client, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control artifact list|export|deliver|get NODE [PATH|ID] [DESTINATION]")
	}
	method, target := args[0], args[1]
	if method == "list" {
		return callPrint(ctx, c, target, "artifacts.list", map[string]any{})
	}
	if len(args) < 3 {
		return errors.New("artifact path or ID is required")
	}
	if method == "export" {
		return callPrint(ctx, c, target, "artifacts.export", map[string]any{"path": args[2]})
	}
	if len(args) < 4 {
		return errors.New("destination is required")
	}
	if method == "deliver" {
		return callPrint(ctx, c, target, "artifacts.deliver", map[string]any{"id": args[2], "target": args[3]})
	}
	if method != "get" {
		return errors.New("unknown artifact command")
	}
	var artifacts []model.Artifact
	if err := c.Call(ctx, target, "artifacts.list", map[string]any{}, &artifacts); err != nil {
		return err
	}
	for _, a := range artifacts {
		if a.ID != args[2] {
			continue
		}
		partial := args[3] + ".partial"
		f, err := os.OpenFile(partial, os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		offset, err := f.Seek(0, io.SeekEnd)
		if err != nil {
			return err
		}
		if offset > a.Size {
			return errors.New("partial file exceeds artifact size; remove it before retrying")
		}
		if err = c.Download(ctx, a, offset, f); err != nil {
			return err
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
			_ = f.Close()
			_ = os.Remove(partial)
			return errors.New("artifact checksum mismatch; partial removed")
		}
		if err = f.Sync(); err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		return os.Rename(partial, args[3])
	}
	return errors.New("artifact not found")
}

func callPrint(ctx context.Context, c client.Client, target, method string, params any) error {
	var result json.RawMessage
	if err := c.Call(ctx, target, method, params, &result); err != nil {
		return err
	}
	printJSON(result)
	return nil
}

func printJSON(value any) { b, _ := json.MarshalIndent(value, "", "  "); fmt.Println(string(b)) }
func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func releaseRepository() string {
	if value, provided := os.LookupEnv("CONTROL_RELEASE_REPO"); provided {
		return value
	}
	return buildinfo.ReleaseRepo
}
func readJSON(value string) (json.RawMessage, error) {
	b := []byte(value)
	var err error
	if strings.HasPrefix(value, "@") {
		b, err = os.ReadFile(strings.TrimPrefix(value, "@"))
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	return b, nil
}

func serve(ctx context.Context, server *http.Server, cert, key string) error {
	result := make(chan error, 1)
	go func() {
		if cert != "" || key != "" {
			result <- server.ListenAndServeTLS(cert, key)
		} else {
			result <- server.ListenAndServe()
		}
	}()
	slog.Info("listening", "address", server.Addr)
	select {
	case err := <-result:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

const usage = `control: peer execution network

Global flags: --config PATH --api URL --token TOKEN --debug
Environment: CONTROL_API, CONTROL_TOKEN

  gateway [--listen ADDRESS] [--data DIR] [--tls-cert PEM --tls-key PEM]
  node --config control.json
  login --gateway URL [--key-stdin]      Save an API key for future commands
  secrets set NAME [--stdin]            Enter a worker-local secret outside chat
  secrets list|delete [NAME]            List names or delete a local secret; no read command
  update [--version TAG]                Update this CLI from GitHub Releases; alias: upgrade
  setup [--gateway URL] --name NAME [--client opencode]  Configure and start this host
  service start|stop|status|uninstall    Manage the installed background node
  service firewall                     Configure Windows node firewall rules (UAC)
  install-mcp CLIENT [--project DIR]    Configure an AI client's MCP server
  install-skill CLIENT [--project DIR]  Install the Control CLI skill
  mcp                                  Serve MCP over stdio using saved credentials
  machines                             List your fleet directly from the gateway
  dashboard [--once] [--json] [--node NAME,...]  Gateway status, fleet activity and resources
  system NODE [--refresh]               Cached or requested system sample
  speedtest NODE [--from WORKER] [--size MiB] [--samples N] [--timeout DURATION] [--json]
  call NODE METHOD [JSON|@file]         Invoke any operation
  exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]
  task start NODE JSON|@file            Submit a durable task
  task get|wait|cancel|logs NODE ID      Inspect or manage a task
  task logs NODE ID [--follow] [--offset BYTES]
  task list NODE                       List tasks owned by this client or local node
  artifact list NODE
  artifact export NODE PATH
  artifact deliver NODE ID DEST_NODE   Peer-to-peer transfer
  artifact get NODE ID LOCAL_PATH      Resumable, checksum-verified download
  tunnel NODE HOST:PORT [--reverse] [--listen ADDRESS]
  tunnel start NODE HOST:PORT --listen ADDRESS [--reverse] [--id ID] [--ttl DURATION]
  tunnel list
  tunnel dispose ID
  clipboard paste NODE [--reverse] [--dir PATH] [--timeout DURATION]
  session                              Inspect this process's client identity and connections

Global flags precede the command. Remote CLI/MCP calls use a local node when
available, otherwise a command-scoped peer using your saved gateway login.
Explicit --api or CONTROL_API selects only that API and disables fallback.
`
