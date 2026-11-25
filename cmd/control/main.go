package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/desktop"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/logfile"
	"github.com/koltyakov/control/internal/model"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	args := os.Args[1:]
	// Installers check declared capabilities, never version strings, after
	// verifying the pinned binary and before changing the machine.
	if len(args) == 1 && args[0] == "__installer-features" {
		_ = json.NewEncoder(os.Stdout).Encode(enrollment.InstallerFeatures{GatewayProxy: true})
		return
	}
	if len(args) > 0 && args[0] == "__desktop" {
		desktop.RunHelper(os.Stdin, os.Stdout, args[1:])
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if len(args) == 4 && args[0] == "__startup-system" {
		log, err := os.OpenFile(filepath.Join(args[2], "startup-change.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, "control startup:", err)
			os.Exit(1)
		}
		err = installation.ApplyStartupChange(ctx, args[1], args[2], args[3])
		if err != nil {
			_, _ = fmt.Fprintln(log, "Control startup migration failed:", err)
		}
		_ = log.Close()
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if len(args) == 2 && args[0] == "__tunnels" {
		logPath := filepath.Join(args[1], "service.log")
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err == nil {
			defer func() { _ = logFile.Close() }()
			os.Stderr = logFile
		}
		logs := make(chan struct{})
		logCtx, stopLogs := context.WithCancel(ctx)
		go func() { defer close(logs); logfile.Maintain(logCtx, logPath) }()
		err = client.RunTunnelService(ctx, args[1])
		stopLogs()
		<-logs
		if err != nil {
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
	if (len(args) == 1 || len(args) == 3) && args[0] == "__update-handoff" {
		if err := runManagedHandoff(ctx, args[1:]...); err != nil {
			if os.Stdout != os.Stderr {
				_, _ = fmt.Fprintf(os.Stdout, "error: %.1024s\n", err.Error())
			}
			fmt.Fprintln(os.Stderr, "control update handoff:", err)
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
		var status shellExitStatus
		if errors.As(err, &status) {
			os.Exit(int(status))
		}
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
	if len(args) == 2 && args[0] == "shell" && (args[1] == "--help" || args[1] == "-h") {
		fmt.Println(shellUsage)
		return nil
	}
	if len(args) > 0 && args[0] == "runbooks" {
		q, err := parseRunbookQuery(args[1:])
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		if q.Node == "" || q.Local || q.Source == "project" || q.Source == "global" {
			return runbooksCLI(ctx, client.Client{}, args[1:])
		}
	}
	if len(args) == 0 {
		fmt.Print(commandHelp(ctx, admin()))
		return nil
	}
	if args[0] == "completion" {
		return completionCLI(os.Stdout, args[1:], func() string { return commandHelp(ctx, admin()) })
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
		userAPI := explicitAPI || os.Getenv("CONTROL_API") != ""
		if args[0] == "shell" && userAPI {
			return errors.New("shell requires account-client routing; remove --api and unset CONTROL_API")
		}
		if !userAPI && cfg.Name != "" {
			c.URL = installation.API(cfg)
		}
		if !explicitToken && os.Getenv("CONTROL_TOKEN") == "" {
			// The node token is a fleet credential. Send it implicitly only to
			// this host's own API or over HTTPS, never to another plain-HTTP host.
			if !userAPI || client.SafeCredentialURL(c.URL) {
				c.Token = cfg.Token
			} else if cfg.Token != "" {
				fmt.Fprintln(os.Stderr, "control: not sending this host's node token to non-loopback plain-HTTP API", c.URL+"; pass --token or CONTROL_TOKEN explicitly")
			}
		}
		switch args[0] {
		case "mcp", "system", "call", "exec", "shell", "task", "artifact", "upload", "tunnel", "session", "clipboard", "dashboard", "speedtest", "diagnostics", "wake", "runbooks", "browser", "doctor":
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
	case "browser":
		return browserCLI(ctx, c, args[1:])
	case "runbooks":
		return runbooksCLI(ctx, c, args[1:])
	case "login":
		return loginCLI(ctx, args[1:], *profile)
	case "secrets":
		var override *string
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "token" {
				override = token
			}
		})
		return secretsCLI(ctx, args[1:], *profile, override)
	case "install-self":
		return installSelfCLI(ctx, args[1:])
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
	case "keys", "users", "audit":
		return adminCLI(ctx, admin(), args)
	case "console":
		return consoleCLI(ctx, admin(), args[1:])
	case "gateway":
		return gatewayCLI(ctx, args[1:], commandArgs, *token)
	case "node":
		return nodeCLI(ctx, args[1:], commandArgs, *token)
	case "mcp":
		return c.MCPServer().Run(ctx, &mcp.StdioTransport{})
	case "machines":
		return machinesCLI(ctx, admin(), args[1:])
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
	case "diagnostics":
		if len(args) != 2 {
			return errors.New("usage: control diagnostics NODE")
		}
		return callPrint(ctx, c, args[1], "diagnostics.get", map[string]any{})
	case "exec":
		return execCLI(ctx, c, args[1:])
	case "shell":
		return shellCLI(ctx, c, args[1:])
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
	case "doctor":
		return doctorCLI(ctx, c, flags, *profile, args[1:])
	case "wake":
		return wakeCLI(ctx, c, args[1:])
	case "clipboard":
		return clipboardCLI(ctx, c, args[1:])
	case "task":
		if len(args) < 3 {
			return errors.New("usage: control task start|get|wait|cancel|logs|list NODE [ID|JSON|@file] [--wait] [--tag TAG]")
		}
		method, target := args[1], args[2]
		if method == "start" {
			return taskStartCLI(ctx, c, args[2:])
		}
		if method == "list" {
			f := flag.NewFlagSet("task list", flag.ContinueOnError)
			limit := f.Int("limit", 100, "maximum task summaries, 1..200")
			cursor := f.String("cursor", "", "nextCursor from the previous page")
			tag := f.String("tag", "", "only tasks carrying this tag; the node must advertise task tags")
			if err := f.Parse(args[3:]); err != nil {
				return err
			}
			if f.NArg() != 0 {
				return errors.New("unexpected task list arguments")
			}
			return callPrint(ctx, c, target, "tasks.page", taskPageParams(*limit, *cursor, *tag))
		}
		if method == "cancel" && len(args) > 3 && strings.HasPrefix(args[3], "-") {
			return taskCancelTaggedCLI(ctx, c, target, args[3:])
		}
		if method == "reconcile" && len(args) == 4 {
			report, err := c.ReconcileWorkflow(ctx, target, args[3])
			if err != nil {
				return err
			}
			printJSON(report)
			return nil
		}
		if len(args) < 4 {
			return errors.New("task ID or specification is required")
		}
		if method == "logs" {
			return logsCLI(ctx, c, target, args[3:])
		}
		f := flag.NewFlagSet("task "+method, flag.ContinueOnError)
		includeSpec := f.Bool("spec", false, "include the submitted specification")
		if err := f.Parse(args[4:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return fmt.Errorf("unexpected task %s arguments", method)
		}
		if method == "wait" {
			task, err := c.Wait(ctx, target, args[3])
			if err != nil {
				return err
			}
			printTask(task, *includeSpec)
			if task.State != "succeeded" {
				return errors.New(task.Error)
			}
			return nil
		}
		switch method {
		case "get", "cancel":
		default:
			return errors.New("unknown task command")
		}
		var task json.RawMessage
		if err := c.Call(ctx, target, "tasks."+method, map[string]any{"id": args[3]}, &task); err != nil {
			return err
		}
		printTask(task, *includeSpec)
		return nil
	case "artifact":
		return artifactCLI(ctx, c, args[1:])
	case "upload":
		return uploadCLI(ctx, c, args[1:])
	case "tunnel":
		return tunnelCLI(ctx, c, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func submit(ctx context.Context, c client.Client, target string, spec model.TaskSpec, wait, includeSpec bool) error {
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	// Reject locally so the error does not suggest a submission may exist.
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID)
	}
	// Print before sending so an interrupted submission can be reconciled.
	fmt.Fprintln(os.Stderr, "task", spec.ID)
	spec, err := resolveTaskPlan(ctx, c, target, spec)
	if err != nil {
		return err
	}
	var task model.Task
	if wait {
		// Overlap the blocking-wait lane setup with submission. Cancelling it
		// on return cannot affect the task.
		prepare, stopPrepare := context.WithCancel(ctx)
		defer stopPrepare()
		go c.PrepareTaskWait(prepare, target)
	}
	if _, err := c.StartTask(ctx, target, spec, &task); err != nil {
		return err
	}
	if wait && !task.Terminal() {
		observed, err := c.Wait(ctx, target, task.ID)
		if err != nil {
			if observed.ID != "" {
				task = observed
			}
			printTask(task, includeSpec)
			return fmt.Errorf("wait for task %s; last known state printed; query this ID before proceeding, do not resubmit automatically: %w", spec.ID, err)
		}
		task = observed
	}
	printTask(task, includeSpec)
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
		f := flag.NewFlagSet("artifact list", flag.ContinueOnError)
		limit := f.Int("limit", model.DefaultArtifactPage, "maximum artifact summaries, 1..200")
		cursor := f.String("cursor", "", "nextCursor from the previous page")
		all := f.Bool("all", false, "print every visible artifact as a full reference")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected artifact list arguments")
		}
		if *all {
			return callPrint(ctx, c, target, "artifacts.list", map[string]any{})
		}
		page, err := c.Artifacts(ctx, client.ArtifactQuery{Node: target, Cursor: *cursor, Limit: *limit})
		if err != nil {
			return err
		}
		printJSON(page)
		return nil
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
	// Older workers ignore the ID filter and return every artifact; the loop
	// still selects the requested one.
	var artifacts []model.Artifact
	if err := c.Call(ctx, target, "artifacts.list", map[string]any{"id": args[2]}, &artifacts); err != nil {
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
		// Hash the retained prefix, then the received bytes as they arrive,
		// instead of reading the whole file again afterwards.
		hash := sha256.New()
		offset, err := io.Copy(hash, f)
		if err != nil {
			return err
		}
		if offset > a.Size {
			return errors.New("partial file exceeds artifact size; remove it before retrying")
		}
		if err = c.Download(ctx, a, offset, io.MultiWriter(f, hash)); err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 && offset > 0 {
			// The retained prefix may belong to other content downloaded to
			// the same path. Refetch once from the start instead of failing.
			if err = f.Truncate(0); err != nil {
				return err
			}
			if _, err = f.Seek(0, io.SeekStart); err != nil {
				return err
			}
			hash.Reset()
			if err = c.Download(ctx, a, 0, io.MultiWriter(f, hash)); err != nil {
				return err
			}
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

// printTask omits the submitted specification unless requested; the caller
// already supplied it, and inline scripts would otherwise repeat on each read.
func printTask(task any, includeSpec bool) {
	data := model.JSON(task)
	if !includeSpec {
		data = client.WithoutTaskSpec(data)
	}
	printJSON(data)
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
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if cert != "" || key != "" {
		if _, err = tls.LoadX509KeyPair(cert, key); err != nil {
			return err
		}
	}
	if err = signalManagedReady(ctx); err != nil {
		return err
	}
	result := make(chan error, 1)
	go func() {
		if cert != "" || key != "" {
			result <- server.ServeTLS(listener, cert, key)
		} else {
			result <- server.Serve(listener)
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
		err := server.Shutdown(shutdown)
		if errors.Is(err, context.DeadlineExceeded) {
			// A request that outlives the grace period must not turn a normal
			// stop into a failure or abandon a pending update handoff.
			slog.Warn("closing HTTP requests still active after shutdown grace period")
			_ = server.Close()
			return nil
		}
		return err
	}
}

const usage = `control: peer execution network

Global flags: --config PATH --api URL --token TOKEN --debug
Environment: CONTROL_API, CONTROL_TOKEN

  gateway [--listen ADDRESS] [--data DIR] [--tls-cert PEM --tls-key PEM]
  node --config control.json
  login --gateway URL [--key-stdin]      Save an API key for future commands
  secrets set NAME [--stdin] [--node WORKER]  Enter a secret outside chat; remote values are encrypted
      --use USES --host PATTERN --plain-http --clear-policy  Replace its use policy with the value
  secrets list [--metadata] [--node WORKER]   List names and optional timestamps and policies, never values
  secrets delete NAME [--node WORKER]        Delete a secret; no value-read command
  secrets policy NAME [--use USES] [--host PATTERN] [--plain-http] [--clear]  Change a local use policy only
  update [--version TAG]                Update this CLI from GitHub Releases; alias: upgrade
  setup [--gateway URL] --name NAME [--client opencode]  Configure and start this host
  service start|stop|status|uninstall    Manage the installed background node
  service firewall                     Configure Windows node firewall rules (UAC)
  install-mcp CLIENT [--project DIR]    Configure an AI client's MCP server
  install-skill CLIENT [--project DIR]  Install the Control CLI skill
  completion bash|zsh|fish|powershell [--login]  Print a shell completion script
  mcp                                  Serve MCP over stdio using saved credentials
  machines [--compact] [--node NAME_OR_ID] List fleet JSON; optionally project or filter it
  dashboard [--once] [--json] [--node NAME,...]  Gateway status, fleet activity and resources
  system NODE [--refresh]               Cached or requested system sample
  speedtest NODE [--from WORKER] [--size MiB] [--samples N] [--timeout DURATION] [--json]
  wake NODE [--from WORKER|local] [--json]
  call NODE METHOD [JSON|@file]         Invoke any operation
  runbooks [--node NAME] [--local]       Discover shared and machine-specific operational instructions
    --source SOURCE --file FILE         Read one complete runbook from its exact origin
  browser run NODE [--session NAME] -- COMMAND [ARG...]
                                       Operate an opt-in worker agent-browser CLI
  browser view NODE [--port 4848] [--listen 127.0.0.1:0]
                                       Forward a worker browser dashboard to a local URL
  exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]
  shell NODE [--] bash|cmd|powershell|pwsh
                                       Open an interactive terminal using your account login
  task start NODE JSON|@file [--wait] [--spec] [--check]
                                       Submit a durable task and optionally wait; --check only reports
                                       whether it would be accepted (advisory, submits nothing)
  task get|wait|cancel NODE ID [--spec] Inspect or manage a task; --spec adds its specification
  task cancel NODE --tag TAG           Cancel your unfinished tasks carrying TAG; lists each outcome
  task logs NODE ID [--follow | --tail] [--offset BYTES]
  task list NODE [--limit N --cursor ID] [--tag TAG]  Page through task summaries, optionally by tag
  task reconcile NODE ID               Inspect a workflow and its stable child IDs
  diagnostics NODE                     Read bounded operational counters
  artifact list NODE [--limit N --cursor C | --all]  Newest-first summaries, or every full reference
  artifact export NODE PATH
  artifact deliver NODE ID DEST_NODE   Peer-to-peer transfer
  artifact get NODE ID LOCAL_PATH      Resumable, checksum-verified download
  upload start NODE LOCAL REMOTE [--id ID --timeout DURATION]  Stream a local file, no overwrite
  upload start NODE LOCAL_DIR REMOTE_DIR --recursive [--skip-unsupported]  Upload a tree into a new directory
  upload resume NODE ID LOCAL [--timeout DURATION]  Explicitly resume a verified partial or directory
  upload status|abort NODE ID          Reconcile publication or abandon staging
  tunnel NODE HOST:PORT [--reverse] [--listen ADDRESS]
  tunnel NODE --socks [--listen ADDRESS] [--max-connections N] [--allow-non-loopback]  Local SOCKS5 proxy through NODE
  tunnel NODE --https-proxy [--listen ADDRESS] [--max-connections N]  Loopback HTTPS/HTTP2 CONNECT proxy through NODE
  tunnel start NODE HOST:PORT --listen ADDRESS [--reverse] [--id ID] [--ttl DURATION]
  tunnel list
  tunnel dispose ID
  clipboard paste NODE [--reverse] [--dir PATH] [--timeout DURATION]
  session                              Inspect this process's client identity and connections
  doctor [NODE] [--json] [--timeout DURATION] [--check-timeout DURATION]
                                       Read-only connection checks with hints; starts no work, grants or leases

Global flags precede the command. Remote CLI/MCP calls use an account client
session with your saved gateway login, even when a local node is running.
Explicit --api or CONTROL_API selects only that node's local authority.
Worker credentials permit discovery, not independent peer execution.
`
