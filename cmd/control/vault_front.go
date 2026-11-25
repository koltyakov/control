package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/browser"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/vaultwire"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// vaultPeer is the frontend's broker channel. It carries the typed operations
// documented on client.VaultPeer; the frontend holds no Control credentials.
type vaultPeer interface {
	Do(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

// vaultRefusedCommands fail closed in vault mode. They need credentials,
// administrative authority, local privileged actions, or data-plane streams
// that the broker channel does not carry.
var vaultRefusedCommands = map[string]string{
	"shell":         "interactive shell streams need the regular account client",
	"upload":        "file uploads stream local bytes to the worker; only upload status|abort NODE ID is available",
	"clipboard":     "clipboard pastes read or write this machine's clipboard",
	"speedtest":     "connection tests stream synthetic bulk traffic",
	"wake":          "Wake-on-LAN inspects local network evidence and may send local packets",
	"dashboard":     "the dashboard reads gateway status with saved credentials",
	"doctor":        "doctor inspects saved credentials and local connectivity",
	"login":         "credentials are managed outside vault frontends",
	"secrets":       "secret management is administrative",
	"keys":          "key administration is unavailable",
	"users":         "user administration is unavailable",
	"audit":         "gateway audit is administrative",
	"console":       "the web console uses administrative credentials",
	"update":        "updates are administrative",
	"upgrade":       "updates are administrative",
	"gateway":       "running a gateway is unavailable",
	"node":          "running a node is unavailable",
	"setup":         "host setup is a local privileged action",
	"enroll":        "enrollment is a local privileged action",
	"service":       "service management is a local privileged action",
	"install-mcp":   "agent configuration is a local privileged action",
	"install-skill": "agent configuration is a local privileged action",
	"install-self":  "installation is a local privileged action",
	"completion":    "completion scripts read administrative help",
}

const vaultFrontUsage = `control vault frontend: requests go through the approved vault broker

  machines [--compact] [--node NAME_OR_ID]
  session
  system NODE [--refresh]
  diagnostics NODE
  call NODE METHOD [JSON|@file]
  exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]
  task start NODE JSON|@file [--wait] [--spec] [--check]
  task get|wait|cancel NODE ID [--spec]
  task cancel NODE --tag TAG
  task logs NODE ID [--follow | --tail] [--offset BYTES]
  task list NODE [--limit N --cursor ID] [--tag TAG]
  task reconcile NODE ID               Inspect a workflow's stable child IDs; never infers notStarted
  upload status|abort NODE ID          Single-file staging only, as the broker's account owner
  artifact list NODE [--limit N --cursor C | --all]
  artifact export NODE PATH
  artifact deliver NODE ID DEST_NODE
  runbooks [--node NAME] [--local] [--source SOURCE --file FILE]
  browser run NODE [--session NAME] -- COMMAND [ARG...]
  browser view NODE [--port 4848] [--listen 127.0.0.1:0]
  tunnel NODE HOST:PORT [--reverse] [--listen ADDRESS]
  tunnel NODE (--socks | --https-proxy) [--listen ADDRESS] [--max-connections N]
  tunnel start NODE HOST:PORT --listen ADDRESS [--reverse] [--id ID] [--ttl DURATION]
  tunnel list
  tunnel dispose ID
  mcp [--scope process]                 Serve MCP over stdio; one approval covers this process

Forwards and retained tunnels are held by the broker. Runbook discovery reads
project and global stores from this host only; node.describe results carry
worker stores only. Shell, upload start/resume, artifact download, clipboard,
speedtest, wake, doctor, dashboard and administrative commands are
unavailable in vault mode.
`

// vaultFrontend runs one regular CLI command through an already authorized
// broker connection. It never reads saved credentials or falls back to them.
func vaultFrontend(ctx context.Context, args []string, peer vaultPeer) error {
	return runVaultFrontend(ctx, args, peer, os.Stdout, os.Stderr)
}

type vaultFront struct {
	v      *client.Vault
	stdout io.Writer
	stderr io.Writer
	// disconnected closes when the broker connection physically ends, which
	// releases every forward bound to it. Nil when the peer cannot report it.
	disconnected <-chan struct{}
}

// vaultWirePeer reports broker wire errors as delivery-aware RPC errors, so
// MCP and CLI output keep the code and never overstate certainty.
type vaultWirePeer struct{ peer vaultPeer }

func (p vaultWirePeer) Do(ctx context.Context, op string, params json.RawMessage) (json.RawMessage, error) {
	result, err := p.peer.Do(ctx, op, params)
	if err != nil {
		return nil, vaultWireError(err)
	}
	return result, nil
}

func vaultWireError(err error) error {
	var wire *vaultwire.Error
	if !errors.As(err, &wire) {
		return err
	}
	outcome := "unknown"
	switch wire.Code {
	case vaultwire.CodeApprovalRequired, vaultwire.CodeApprovalDenied, vaultwire.CodeApprovalFailed:
		// The broker refuses these before dispatch. It rewrites handler errors
		// carrying these codes to op_failed, so they cannot be spoofed here.
		outcome = "not_sent"
	default:
		// The broker forwards a handler's known pre-send refusal. Only
		// not_sent narrows certainty; any other or absent outcome stays unknown.
		if wire.Outcome == vaultwire.OutcomeNotSent {
			outcome = "not_sent"
		}
	}
	return &model.RPCError{Code: wire.Code, Outcome: outcome, Message: wire.Error(), Cause: err}
}

func runVaultFrontend(ctx context.Context, args []string, peer vaultPeer, stdout, stderr io.Writer) error {
	if peer == nil {
		return errors.New("vault mode requires a broker connection")
	}
	v, err := client.NewVault(vaultWirePeer{peer: peer})
	if err != nil {
		return err
	}
	cli := vaultFront{v: v, stdout: stdout, stderr: stderr}
	if session, ok := peer.(interface{ Done() <-chan struct{} }); ok {
		cli.disconnected = session.Done()
	}
	if len(args) == 0 {
		_, err := fmt.Fprint(stdout, vaultFrontUsage)
		return err
	}
	switch args[0] {
	case "help", "--help", "-h":
		_, err := fmt.Fprint(stdout, vaultFrontUsage)
		return err
	case "version":
		if len(args) > 1 && args[1] == "--json" {
			return cli.print(buildinfo.Current())
		}
		_, err := fmt.Fprintln(stdout, "control", buildinfo.Version)
		return err
	case "mcp":
		return cli.mcp(ctx, args[1:])
	case "machines":
		return cli.machines(ctx, args[1:])
	case "session":
		if len(args) != 1 {
			return errors.New("usage: control session")
		}
		// The caller authorized this process's connection before routing here.
		info, err := v.Session(ctx, client.VaultScopeProcess)
		if err != nil {
			return err
		}
		return cli.print(info)
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
		return cli.callPrint(ctx, args[1], "system.info", map[string]any{"refresh": *refresh})
	case "diagnostics":
		if len(args) != 2 {
			return errors.New("usage: control diagnostics NODE")
		}
		return cli.callPrint(ctx, args[1], "diagnostics.get", map[string]any{})
	case "call":
		if len(args) < 3 || len(args) > 4 {
			return errors.New("usage: control call NODE METHOD [JSON|@file]")
		}
		params := json.RawMessage(`{}`)
		if len(args) == 4 {
			if params, err = cli.readJSON(ctx, args[3]); err != nil {
				return err
			}
		}
		return cli.callPrint(ctx, args[1], args[2], params)
	case "exec":
		return cli.exec(ctx, args[1:])
	case "task":
		return cli.task(ctx, args[1:])
	case "artifact":
		return cli.artifact(ctx, args[1:])
	case "runbooks":
		q, err := parseRunbookQuery(args[1:])
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		if err != nil {
			return err
		}
		catalog, err := v.Runbooks(ctx, q)
		if err != nil {
			return err
		}
		return cli.print(catalog)
	case "browser":
		return cli.browser(ctx, args[1:])
	case "tunnel":
		return cli.tunnel(ctx, args[1:])
	case "upload":
		if len(args) == 4 && (args[1] == "status" || args[1] == "abort") {
			result, err := v.UploadState(ctx, args[2], args[3], args[1] == "abort")
			if err != nil {
				return err
			}
			return cli.print(result)
		}
	}
	if reason, ok := vaultRefusedCommands[args[0]]; ok {
		return fmt.Errorf("%s is unavailable in vault mode: %s", args[0], reason)
	}
	return fmt.Errorf("unknown command %q", args[0])
}

func (cli vaultFront) print(value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(cli.stdout, string(b))
	return err
}

func (cli vaultFront) printTask(task any, includeSpec bool) error {
	data := model.JSON(task)
	if !includeSpec {
		data = client.WithoutTaskSpec(data)
	}
	return cli.print(data)
}

func (cli vaultFront) callPrint(ctx context.Context, target, method string, params any) error {
	var result json.RawMessage
	if err := cli.v.Call(ctx, target, method, params, &result); err != nil {
		return err
	}
	return cli.print(result)
}

// readJSON reads inline JSON or @file input within the broker payload limit,
// so an oversized request fails before anything is sent. An @file is opened
// only after a live-grant check, so a pending or denied connection cannot
// probe which local files exist. The check is sent once.
func (cli vaultFront) readJSON(ctx context.Context, value string) (json.RawMessage, error) {
	b := []byte(value)
	if name, ok := strings.CutPrefix(value, "@"); ok {
		if err := cli.v.Check(ctx); err != nil {
			return nil, err
		}
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		if b, err = io.ReadAll(io.LimitReader(f, client.VaultMaxPayload+1)); err != nil {
			return nil, err
		}
	}
	if len(b) > client.VaultMaxPayload {
		return nil, fmt.Errorf("JSON input exceeds %d bytes; nothing was sent", client.VaultMaxPayload)
	}
	if !json.Valid(b) {
		return nil, errors.New("invalid JSON")
	}
	return b, nil
}

func (cli vaultFront) mcp(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("mcp", flag.ContinueOnError)
	scope := f.String("scope", string(client.VaultScopeProcess), "what one approval covers; only process is supported")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control mcp [--scope process]")
	}
	server, err := cli.v.MCPServer(ctx, client.VaultMCPOptions{Scope: client.VaultScope(*scope)})
	if err != nil {
		return err
	}
	return server.Run(ctx, &mcp.StdioTransport{})
}

func (cli vaultFront) machines(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("machines", flag.ContinueOnError)
	compact := f.Bool("compact", false, "print compact machine summaries")
	node := f.String("node", "", "exact machine name or stable ID")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New("usage: control machines [--compact] [--node NAME_OR_ID]")
	}
	data, err := cli.v.Machines(ctx, client.MachineQuery{Node: *node, Compact: *compact})
	if err != nil {
		return err
	}
	return cli.print(data)
}

func (cli vaultFront) exec(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control exec NODE [--detach] [--id ID] [--timeout DURATION] [--] COMMAND [ARG...]")
	}
	f := flag.NewFlagSet("exec", flag.ContinueOnError)
	detach := f.Bool("detach", false, "return after durable acceptance instead of waiting")
	id := f.String("id", "", "stable task ID for submission recovery")
	timeout := f.Duration("timeout", time.Hour, "task timeout including queue/input time, up to 24h")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() == 0 || *timeout < time.Second || *timeout > 24*time.Hour {
		return errors.New("provide a command and a timeout between 1s and 24h")
	}
	command := f.Args()
	spec := model.TaskSpec{ID: *id, Capability: "exec.run", TimeoutSeconds: int(*timeout / time.Second), Args: model.JSON(map[string]any{"command": command[0], "args": command[1:]})}
	return cli.submit(ctx, args[0], spec, !*detach, false)
}

// submit mirrors submit in main.go: the ID is printed before sending so an
// interrupted submission can be reconciled, and nothing is resubmitted.
func (cli vaultFront) submit(ctx context.Context, target string, spec model.TaskSpec, wait, includeSpec bool) error {
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID)
	}
	_, _ = fmt.Fprintln(cli.stderr, "task", spec.ID)
	var task model.Task
	if _, err := cli.v.StartTask(ctx, target, spec, &task); err != nil {
		return err
	}
	if wait && !task.Terminal() {
		observed, err := cli.v.Wait(ctx, target, task.ID)
		if err != nil {
			if observed.ID != "" {
				task = observed
			}
			_ = cli.printTask(task, includeSpec)
			return fmt.Errorf("wait for task %s; last known state printed; query this ID before proceeding, do not resubmit automatically: %w", spec.ID, err)
		}
		task = observed
	}
	if err := cli.printTask(task, includeSpec); err != nil {
		return err
	}
	if task.Terminal() && task.State != "succeeded" {
		return errors.New(task.Error)
	}
	return nil
}

func (cli vaultFront) task(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control task start|get|wait|cancel|logs|list NODE [ID|JSON|@file] [--wait] [--tag TAG]")
	}
	method, target := args[0], args[1]
	switch method {
	case "start":
		return cli.taskStart(ctx, args[1:])
	case "list":
		f := flag.NewFlagSet("task list", flag.ContinueOnError)
		limit := f.Int("limit", 100, "maximum task summaries, 1..200")
		cursor := f.String("cursor", "", "nextCursor from the previous page")
		tag := f.String("tag", "", "only tasks carrying this tag; the node must advertise task tags")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected task list arguments")
		}
		return cli.callPrint(ctx, target, "tasks.page", taskPageParams(*limit, *cursor, *tag))
	case "reconcile":
		if len(args) != 3 {
			return errors.New("usage: control task reconcile NODE ID")
		}
		report, err := cli.v.ReconcileWorkflow(ctx, target, args[2])
		if err != nil {
			return err
		}
		return cli.print(report)
	}
	if method == "cancel" && len(args) > 2 && strings.HasPrefix(args[2], "-") {
		return cli.cancelTagged(ctx, target, args[2:])
	}
	if len(args) < 3 {
		return errors.New("task ID or specification is required")
	}
	if method == "logs" {
		return cli.logs(ctx, target, args[2:])
	}
	if method != "get" && method != "cancel" && method != "wait" {
		return errors.New("unknown task command")
	}
	f := flag.NewFlagSet("task "+method, flag.ContinueOnError)
	includeSpec := f.Bool("spec", false, "include the submitted specification")
	if err := f.Parse(args[3:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected task %s arguments", method)
	}
	if method == "wait" {
		task, err := cli.v.Wait(ctx, target, args[2])
		if err != nil {
			return err
		}
		if err := cli.printTask(task, *includeSpec); err != nil {
			return err
		}
		if task.State != "succeeded" {
			return errors.New(task.Error)
		}
		return nil
	}
	var task json.RawMessage
	if err := cli.v.Call(ctx, target, "tasks."+method, map[string]any{"id": args[2]}, &task); err != nil {
		return err
	}
	return cli.printTask(task, *includeSpec)
}

func (cli vaultFront) taskStart(ctx context.Context, args []string) error {
	const usage = "usage: control task start NODE JSON|@file [--wait] [--spec] [--check]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	f := flag.NewFlagSet("task start", flag.ContinueOnError)
	wait := f.Bool("wait", false, "wait for the task result after durable acceptance")
	includeSpec := f.Bool("spec", false, "include the submitted specification in the printed task")
	check := f.Bool("check", false, "report whether the node would accept the specification, without submitting it")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() == 0 {
		return errors.New(usage)
	}
	value := f.Arg(0)
	if err := f.Parse(f.Args()[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return errors.New(usage)
	}
	b, err := cli.readJSON(ctx, value)
	if err != nil {
		return err
	}
	var spec model.TaskSpec
	if err = json.Unmarshal(b, &spec); err != nil {
		return err
	}
	if !*check {
		return cli.submit(ctx, args[0], spec, *wait, *includeSpec)
	}
	if *wait || *includeSpec {
		return errors.New("--check submits nothing and cannot be combined with --wait or --spec")
	}
	var report model.TaskCheck
	if err := cli.v.Call(ctx, args[0], model.TaskCheckMethod, spec, &report); err != nil {
		return err
	}
	if err := cli.print(report); err != nil {
		return err
	}
	if !report.OK {
		return errors.New("task check failed; nothing was submitted, and the report is advisory")
	}
	return nil
}

func (cli vaultFront) cancelTagged(ctx context.Context, target string, args []string) error {
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
	if err := cli.v.Call(ctx, target, model.CancelTaggedTasks, map[string]any{"tag": *tag}, &result); err != nil {
		return err
	}
	if err := cli.print(result); err != nil {
		return err
	}
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

func (cli vaultFront) logs(ctx context.Context, target string, args []string) error {
	f := flag.NewFlagSet("task logs", flag.ContinueOnError)
	follow := f.Bool("follow", false, "poll logs until completion or cancellation")
	offset := f.Int64("offset", 0, "starting byte offset")
	tail := f.Bool("tail", false, "read the retained end of a log that exceeded the head limit, by absolute offset")
	if err := f.Parse(args[1:]); err != nil {
		return err
	}
	if f.NArg() != 0 || *offset < 0 || (*tail && *follow) {
		return errors.New("usage: control task logs NODE ID [--follow | --tail] [--offset BYTES]")
	}
	if *follow {
		return cli.v.FollowLogs(ctx, target, args[0], *offset, cli.stdout)
	}
	params := map[string]any{"id": args[0], "offset": *offset}
	if *tail {
		params["tail"] = true
	}
	return cli.callPrint(ctx, target, "tasks.logs", params)
}

func (cli vaultFront) artifact(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("usage: control artifact list|export|deliver NODE [PATH|ID] [DESTINATION]")
	}
	method, target := args[0], args[1]
	switch method {
	case "list":
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
			return cli.callPrint(ctx, target, "artifacts.list", map[string]any{})
		}
		page, err := cli.v.Artifacts(ctx, client.ArtifactQuery{Node: target, Cursor: *cursor, Limit: *limit})
		if err != nil {
			return err
		}
		return cli.print(page)
	case "export":
		if len(args) != 3 {
			return errors.New("usage: control artifact export NODE PATH")
		}
		return cli.callPrint(ctx, target, "artifacts.export", map[string]any{"path": args[2]})
	case "deliver":
		if len(args) != 4 {
			return errors.New("usage: control artifact deliver NODE ID DEST_NODE")
		}
		return cli.callPrint(ctx, target, "artifacts.deliver", map[string]any{"id": args[2], "target": args[3]})
	case "get":
		return errors.New("artifact get is unavailable in vault mode: downloads stream bytes; deliver the artifact to a worker instead")
	}
	return errors.New("unknown artifact command")
}

func (cli vaultFront) browser(ctx context.Context, args []string) error {
	const usage = "usage: control browser run NODE [--session NAME] -- COMMAND [ARG...] | control browser view NODE [--port 4848] [--listen 127.0.0.1:0]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	switch args[0] {
	case "run":
		f := flag.NewFlagSet("browser run", flag.ContinueOnError)
		session := f.String("session", "", "browser session in the worker's private owner namespace")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		request := browser.Request{Session: *session, Args: f.Args()}
		if _, err := browser.Decode(model.JSON(request)); err != nil {
			return err
		}
		return cli.callPrint(ctx, args[1], "browser.run", request)
	case "view":
		f := flag.NewFlagSet("browser view", flag.ContinueOnError)
		port := f.Int("port", 4848, "already running agent-browser dashboard port on the worker")
		listen := f.String("listen", "127.0.0.1:0", "loopback listener on the broker host")
		if err := f.Parse(args[2:]); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New(usage)
		}
		info, err := cli.v.StartBrowserView(ctx, client.BrowserViewSpec{Node: args[1], Port: *port, Listen: *listen})
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(cli.stdout, info.URL)
		_, _ = fmt.Fprintln(cli.stderr, "Interactive worker browser dashboard held by the vault broker. Ctrl+C closes this forward, not the browser or dashboard.")
		return cli.holdForward(ctx, info.ID)
	}
	return errors.New(usage)
}

func (cli vaultFront) tunnel(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "start", "list", "dispose", "stop":
			return cli.persistentTunnel(ctx, args)
		}
	}
	const usage = "usage: control tunnel NODE HOST:PORT [--reverse] [--listen ADDRESS], or control tunnel NODE (--socks | --https-proxy) [--listen ADDRESS] [--max-connections N]"
	if len(args) < 2 {
		return errors.New(usage)
	}
	address, flags := args[1], args[2:]
	if strings.HasPrefix(address, "-") {
		address, flags = "", args[1:]
	}
	f := flag.NewFlagSet("tunnel", flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:0", "listen address on the broker host, or on the remote machine with --reverse")
	reverse := f.Bool("reverse", false, "forward a remote listener to HOST:PORT on the broker host")
	socks := f.Bool("socks", false, "run a SOCKS5 proxy on the broker host; NODE resolves and dials each requested host")
	httpsProxy := f.Bool("https-proxy", false, "run a loopback HTTPS proxy on the broker host using HTTP/2 CONNECT")
	maxConnections := f.Int("max-connections", 0, fmt.Sprintf("proxy connection limit (1..%d; zero selects %d)", client.MaxProxyConnections, client.DefaultProxyConnections))
	allowNonLoopback := f.Bool("allow-non-loopback", false, "permit a non-loopback --socks listener, an unauthenticated open proxy into the fleet")
	if err := f.Parse(flags); err != nil {
		return err
	}
	if f.NArg() != 0 || (address == "") != (*socks || *httpsProxy) {
		return errors.New(usage)
	}
	info, err := cli.v.StartForward(ctx, client.ForwardSpec{Node: args[0], Address: address, Listen: *listen, Reverse: *reverse, SOCKS: *socks, HTTPSProxy: *httpsProxy, AllowNonLoopback: *allowNonLoopback, MaxConnections: *maxConnections})
	if err != nil {
		return err
	}
	switch {
	case *httpsProxy:
		_, _ = fmt.Fprintf(cli.stderr, "HTTPS proxy listening on %s through %s (HTTP/2, connection limit %d), held by the vault broker\n", info.ProxyURL, args[0], info.ConnectionLimit)
		_, _ = fmt.Fprintln(cli.stderr, "Proxy SPKI SHA-256:", info.ProxySPKI)
	case *socks:
		_, _ = fmt.Fprintf(cli.stderr, "SOCKS5 proxy listening on %s through %s (connection limit %d), held by the vault broker\n", info.Listen, args[0], info.ConnectionLimit)
	case *reverse:
		_, _ = fmt.Fprintln(cli.stderr, "listening on", args[0], info.Listen, "forwarding to broker-host", address)
	default:
		_, _ = fmt.Fprintln(cli.stderr, "listening on", info.Listen, "(vault broker)")
	}
	return cli.holdForward(ctx, info.ID)
}

// vaultForwardPoll is how often a held forward is checked for completion.
var vaultForwardPoll = 2 * time.Second

// holdForward keeps a broker forward for the caller's lifetime. Only
// management requests cross the broker channel; traffic flows through the
// broker's listener unchanged and never depends on these observations.
//
// It returns only when the caller ends the command, the broker connection
// physically closes, or a successful listing shows the forward has ended. A
// failed observation, including a revoked or expired grant, never ends the
// command: exiting would close the connection and stop a forward that is
// still serving. After such a failure the grant is uncertain, so polling stops
// and no stop request is sent; closing the connection releases the forward.
func (cli vaultFront) holdForward(ctx context.Context, id string) error {
	var rejected uint64
	var lastError string
	observable := true
	defer func() {
		if rejected != 0 {
			_, _ = fmt.Fprintln(cli.stderr, "Proxy capacity rejections:", rejected)
		}
	}()
	ticker := time.NewTicker(vaultForwardPoll)
	defer ticker.Stop()
	for {
		poll := ticker.C
		if !observable {
			poll = nil
		}
		select {
		case <-ctx.Done():
			if observable {
				cli.stopForward(ctx, id)
			}
			return nil
		case <-cli.disconnected:
			return fmt.Errorf("vault broker connection closed; the broker released forward %s with it", id)
		case <-poll:
		}
		forwards, err := cli.v.Forwards(ctx)
		if err != nil {
			if ctx.Err() != nil {
				continue // The caller ended the command during the listing.
			}
			if errors.Is(err, vaultwire.ErrClosed) {
				return fmt.Errorf("vault broker connection closed; the broker released forward %s with it", id)
			}
			observable = false
			_, _ = fmt.Fprintf(cli.stderr, "cannot observe forward %s: %v\nIt stays open until this command ends or the broker connection closes; it will not be stopped by request.\n", id, err)
			continue
		}
		active := false
		for _, forward := range forwards {
			if forward.ID != id {
				continue
			}
			active = true
			rejected = forward.CapacityRejections
			if forward.LastError != "" && forward.LastError != lastError {
				lastError = forward.LastError
				_, _ = fmt.Fprintln(cli.stderr, "forward", id, "last error:", forward.LastError)
			}
		}
		if !active {
			if lastError != "" {
				return fmt.Errorf("forward %s is no longer held by the vault broker: %s", id, lastError)
			}
			return fmt.Errorf("forward %s is no longer held by the vault broker", id)
		}
	}
}

// stopForward closes a forward after the caller ends the command while its
// grant was last observed healthy. A failure is reported, never retried.
func (cli vaultFront) stopForward(ctx context.Context, id string) {
	stop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := cli.v.StopForward(stop, id); err != nil {
		_, _ = fmt.Fprintln(cli.stderr, "stop forward", id+":", err, "(closing the broker connection releases it)")
	}
}

func (cli vaultFront) persistentTunnel(ctx context.Context, args []string) error {
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: control tunnel list")
		}
		infos, err := cli.v.PersistentTunnels(ctx)
		if err != nil {
			return err
		}
		return cli.print(infos)
	case "dispose", "stop":
		if len(args) != 2 {
			return errors.New("usage: control tunnel dispose ID")
		}
		if err := cli.v.DisposePersistentTunnel(ctx, args[1]); err != nil {
			return err
		}
		return cli.print(map[string]any{"id": args[1], "disposed": true})
	case "start":
		if len(args) < 3 {
			return errors.New("usage: control tunnel start NODE HOST:PORT --listen HOST:PORT [--reverse] [--id ID] [--ttl DURATION]")
		}
		f := flag.NewFlagSet("tunnel start", flag.ContinueOnError)
		listen := f.String("listen", "", "fixed listener address, on the remote machine with --reverse")
		reverse := f.Bool("reverse", false, "forward a remote listener to HOST:PORT on the broker host")
		id := f.String("id", "", "stable tunnel ID for reconciliation")
		ttl := f.Duration("ttl", 0, "expire after this duration; zero keeps the tunnel until disposed")
		if err := f.Parse(args[3:]); err != nil {
			return err
		}
		if f.NArg() != 0 || *ttl < 0 || (*ttl != 0 && (*ttl < time.Second || *ttl%time.Second != 0)) {
			return errors.New("unexpected arguments or TTL; use whole seconds, or zero for no expiry")
		}
		if *id == "" {
			*id = identity.NewID()
		}
		_, _ = fmt.Fprintln(cli.stderr, "tunnel", *id)
		info, err := cli.v.StartPersistentTunnel(ctx, client.PersistentTunnelSpec{ID: *id, TTLSeconds: int64(*ttl / time.Second), ForwardSpec: client.ForwardSpec{Node: args[1], Address: args[2], Listen: *listen, Reverse: *reverse}})
		if err != nil {
			return fmt.Errorf("start tunnel %s; list tunnels to reconcile before retrying: %w", *id, err)
		}
		return cli.print(info)
	}
	return errors.New("unknown persistent tunnel command")
}
