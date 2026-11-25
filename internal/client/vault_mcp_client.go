package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
	"github.com/koltyakov/control/internal/upload"
)

// VaultPeer is a vault frontend's channel to the broker that holds the
// account credentials. The frontend never holds them and never falls back to
// saved credentials. Do sends one management or control-plane operation and
// returns its JSON result. Errors should be *model.RPCError so their code and
// delivery outcome survive; any other error is reported with an unknown outcome.
// Do must not replay a request itself.
type VaultPeer interface {
	Do(ctx context.Context, operation string, params json.RawMessage) (json.RawMessage, error)
}

// Broker operations. Only these are sent; there is no generic passthrough.
const (
	VaultOpCall          = "call"           // VaultCall -> method result
	VaultOpMachines      = "machines"       // MachineQuery -> fleet JSON
	VaultOpSession       = "session"        // {} -> broker SessionInfo
	VaultOpForwardStart  = "forward.start"  // ForwardSpec -> ForwardInfo
	VaultOpForwardList   = "forward.list"   // {} -> []ForwardInfo
	VaultOpForwardStop   = "forward.stop"   // {"id"} -> any
	VaultOpTunnelStart   = "tunnel.start"   // PersistentTunnelSpec -> PersistentTunnelInfo
	VaultOpTunnelList    = "tunnel.list"    // {} -> []PersistentTunnelInfo
	VaultOpTunnelDispose = "tunnel.dispose" // {"id"} -> any
	VaultOpCheck         = "check"          // {} -> {"granted":true}; no I/O
)

// VaultApprovalRequired is the error code a broker returns, with outcome
// not_sent and an approval code in the message, while the connection waits
// for the user's approval. Nothing was sent; the frontend never replays it.
const VaultApprovalRequired = "approval_required"

// Broker authorization outcomes. They describe the connection's grant, not
// the requested operation, and are never masked as a source or step error.
const (
	vaultApprovalDenied = "approval_denied"
	vaultApprovalFailed = "approval_failed"
	vaultGrantRevoked   = "grant_revoked"
	vaultGrantExpired   = "grant_expired"
)

// VaultAuthorizationError reports whether err is a broker refusal or loss of
// this connection's grant.
func VaultAuthorizationError(err error) bool {
	var rpc *model.RPCError
	if !errors.As(err, &rpc) {
		return false
	}
	switch rpc.Code {
	case VaultApprovalRequired, vaultApprovalDenied, vaultApprovalFailed, vaultGrantRevoked, vaultGrantExpired:
		return true
	}
	return false
}

// VaultMaxPayload bounds one request and one response on the broker channel,
// matching the local node API response limit.
const VaultMaxPayload = 8 << 20

// vaultMaxMethod bounds a method name sent through the broker.
const vaultMaxMethod = 256

// VaultCall is the broker's "call" request. The broker performs it with the
// shared client semantics of Client.Call: node selection, workflow target
// resolution, task-tag checks and delegation preparation. Its node.describe
// runbook catalog holds worker stores only: the broker never reads its host's
// project or global stores, so that catalog never shows a local origin.
type VaultCall struct {
	Target string          `json:"target"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// Vault routes CLI and MCP requests through a VaultPeer. It holds no
// credentials, opens no data-plane connections and runs no local privileged
// actions; forwards and retained tunnels are owned by the broker.
type Vault struct {
	peer VaultPeer
}

// Check confirms a live grant with the broker's no-I/O check operation. Call
// it before local work that no broker operation gates, such as reading this
// host's runbook stores or a local request file. It is sent once.
func (v *Vault) Check(ctx context.Context) error {
	return v.do(ctx, VaultOpCheck, nil, nil)
}

func NewVault(peer VaultPeer) (*Vault, error) {
	if peer == nil {
		return nil, errors.New("vault mode requires a broker connection")
	}
	return &Vault{peer: peer}, nil
}

func vaultNotSent(err error) error { return model.OperationError(err, "not_sent") }

func (v *Vault) do(ctx context.Context, operation string, params any, result any) error {
	raw := json.RawMessage(`{}`)
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return vaultNotSent(err)
		}
		raw = b
	}
	if len(raw) > VaultMaxPayload {
		return vaultNotSent(fmt.Errorf("vault request exceeds %d bytes; nothing was sent", VaultMaxPayload))
	}
	response, err := v.peer.Do(ctx, operation, raw)
	if err != nil {
		return err
	}
	if len(response) > VaultMaxPayload {
		// The broker already performed the operation; only the reply is refused.
		return model.OperationError(fmt.Errorf("vault broker response exceeds %d bytes", VaultMaxPayload), "unknown")
	}
	if result == nil {
		return nil
	}
	if raw, ok := result.(*json.RawMessage); ok {
		*raw = append((*raw)[:0], response...)
		return nil
	}
	if err := json.Unmarshal(response, result); err != nil {
		return model.OperationError(fmt.Errorf("decode vault broker response: %w", err), "unknown")
	}
	return nil
}

// ValidateVaultMethod is the broker's method policy, also used by the CLI and
// MCP frontends to refuse unsupported calls before sending them.
func ValidateVaultMethod(method string) error {
	switch {
	case method == "":
		return errors.New("method is required")
	case len(method) > vaultMaxMethod:
		return fmt.Errorf("method exceeds %d bytes", vaultMaxMethod)
	case method == model.ShellOpenMethod:
		return errors.New("interactive shell streams are unavailable in vault mode")
	case method == "tcp.open" || method == "tcp.listen" || method == "tcp.accept":
		return errors.New("raw TCP streams are unavailable through vault calls; use forward or tunnel management")
	case method == "artifacts.open" || method == upload.OpenMethod || method == clipboard.OpenMethod || method == clipboard.PasteMethod ||
		method == model.ConnectionOpenMethod || method == model.ConnectionTestMethod:
		return fmt.Errorf("%s streams bytes through this client and is unavailable in vault mode", method)
	case method == "access.grant":
		return errors.New("the vault broker prepares instruction-bound grants; direct grant creation is refused")
	case strings.HasPrefix(method, "secrets.") || strings.HasPrefix(method, "update."):
		return fmt.Errorf("administrative method %s is unavailable in vault mode", method)
	}
	return nil
}

// Call sends one node method through the broker. Refusals are reported before
// anything is sent.
func (v *Vault) Call(ctx context.Context, target, method string, params any, result any) error {
	if err := ValidateVaultMethod(method); err != nil {
		return vaultNotSent(fmt.Errorf("%w; nothing was sent", err))
	}
	raw := json.RawMessage(`{}`)
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			return vaultNotSent(err)
		}
		if !bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
			raw = b
		}
	}
	return v.do(ctx, VaultOpCall, VaultCall{Target: target, Method: method, Params: raw}, result)
}

// StartTask assigns and validates the task ID before sending, so a lost reply
// can be reconciled with tasks.get instead of being resubmitted. The broker
// resolves workflow targets and checks task tags as Client.StartTask does.
func (v *Vault) StartTask(ctx context.Context, target string, spec model.TaskSpec, result any) (string, error) {
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	if !model.TaskIDPattern.MatchString(spec.ID) {
		return "", vaultNotSent(fmt.Errorf("invalid task ID %q: use 1-128 letters, digits, '_' or '-'; nothing was submitted", spec.ID))
	}
	if err := v.Call(ctx, target, "tasks.start", spec, result); err != nil {
		var rpc *model.RPCError
		if errors.As(err, &rpc) && (rpc.Outcome == "not_sent" || rpc.Code == VaultApprovalRequired) {
			return spec.ID, err
		}
		return spec.ID, &TaskSubmitError{ID: spec.ID, Err: err}
	}
	return spec.ID, nil
}

// Wait returns when the task is terminal, as Client.Wait does.
func (v *Vault) Wait(ctx context.Context, target, id string) (model.Task, error) {
	const interval = 250 * time.Millisecond
	var task model.Task
	for {
		started := time.Now()
		var observed model.Task
		if err := v.Call(ctx, target, "tasks.get", map[string]any{"id": id, "waitSeconds": taskWaitSeconds}, &observed); err != nil {
			return task, err
		}
		task = observed
		if task.Terminal() {
			return task, nil
		}
		if remaining := interval - time.Since(started); remaining > 0 {
			select {
			case <-ctx.Done():
				return task, ctx.Err()
			case <-time.After(remaining):
			}
		} else if err := ctx.Err(); err != nil {
			return task, err
		}
	}
}

// FollowLogs polls bounded tasks.logs reads until the task is terminal.
func (v *Vault) FollowLogs(ctx context.Context, target, id string, offset int64, dest io.Writer) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		var logs struct {
			Text     string `json:"text"`
			Offset   int64  `json:"offset"`
			Terminal bool   `json:"terminal"`
		}
		if err := v.Call(ctx, target, "tasks.logs", map[string]any{"id": id, "offset": offset}, &logs); err != nil {
			return err
		}
		if logs.Offset < offset || (logs.Text != "" && logs.Offset == offset) {
			return errors.New("log offset moved backwards")
		}
		if _, err := io.WriteString(dest, logs.Text); err != nil {
			return err
		}
		offset = logs.Offset
		if logs.Terminal && logs.Text == "" {
			return nil
		}
		if logs.Text != "" {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// Artifacts pages a node's artifacts with the same rules as Client.Artifacts.
func (v *Vault) Artifacts(ctx context.Context, q ArtifactQuery) (model.ArtifactPage, error) {
	if q.Node == "" {
		return model.ArtifactPage{}, errors.New("artifact listing requires a node")
	}
	limit := q.Limit
	if limit == 0 {
		limit = model.DefaultArtifactPage
	}
	params := map[string]any{"limit": limit}
	if q.Cursor != "" {
		params["cursor"] = q.Cursor
	}
	var raw json.RawMessage
	if err := v.Call(ctx, q.Node, "artifacts.list", params, &raw); err != nil {
		return model.ArtifactPage{}, err
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		var page model.ArtifactPage
		err := json.Unmarshal(raw, &page)
		return page, err
	}
	var artifacts []model.Artifact
	if err := json.Unmarshal(raw, &artifacts); err != nil {
		return model.ArtifactPage{}, err
	}
	node := q.Node
	if len(artifacts) > 0 && artifacts[0].Node != "" {
		node = artifacts[0].Node
	}
	return model.PageArtifacts(node, artifacts, q.Cursor, limit)
}

func (v *Vault) Machines(ctx context.Context, q MachineQuery) (json.RawMessage, error) {
	if q.Node != "" && len(q.Node) > vaultMaxMethod {
		return nil, vaultNotSent(errors.New("node name is too long"))
	}
	var data json.RawMessage
	err := v.do(ctx, VaultOpMachines, q, &data)
	return data, err
}

// VaultSession describes a vault frontend: the broker's client session and the
// truthful scope one approval covers.
type VaultSession struct {
	Mode          string          `json:"mode"`
	ApprovalScope VaultScope      `json:"approvalScope,omitempty"`
	Broker        json.RawMessage `json:"broker"`
}

func (v *Vault) Session(ctx context.Context, scope VaultScope) (VaultSession, error) {
	var broker json.RawMessage
	if err := v.do(ctx, VaultOpSession, nil, &broker); err != nil {
		return VaultSession{}, err
	}
	return VaultSession{Mode: "vault", ApprovalScope: scope, Broker: broker}, nil
}

func (v *Vault) StartForward(ctx context.Context, spec ForwardSpec) (ForwardInfo, error) {
	var info ForwardInfo
	if spec.Node == "" {
		return info, vaultNotSent(errors.New("forwarding requires a target machine"))
	}
	err := v.do(ctx, VaultOpForwardStart, spec, &info)
	return info, err
}

func (v *Vault) Forwards(ctx context.Context) ([]ForwardInfo, error) {
	infos := []ForwardInfo{}
	err := v.do(ctx, VaultOpForwardList, nil, &infos)
	return infos, err
}

func (v *Vault) StopForward(ctx context.Context, id string) error {
	if id == "" {
		return vaultNotSent(errors.New("forward ID is required"))
	}
	return v.do(ctx, VaultOpForwardStop, map[string]string{"id": id}, nil)
}

func (v *Vault) StartPersistentTunnel(ctx context.Context, spec PersistentTunnelSpec) (PersistentTunnelInfo, error) {
	var info PersistentTunnelInfo
	if spec.ID == "" {
		return info, vaultNotSent(errors.New("retained tunnels require an explicit id for reconciliation"))
	}
	err := v.do(ctx, VaultOpTunnelStart, spec, &info)
	return info, err
}

func (v *Vault) PersistentTunnels(ctx context.Context) ([]PersistentTunnelInfo, error) {
	infos := []PersistentTunnelInfo{}
	err := v.do(ctx, VaultOpTunnelList, nil, &infos)
	return infos, err
}

func (v *Vault) DisposePersistentTunnel(ctx context.Context, id string) error {
	if id == "" {
		return vaultNotSent(errors.New("tunnel ID is required"))
	}
	return v.do(ctx, VaultOpTunnelDispose, map[string]string{"id": id}, nil)
}

// StartBrowserView validates the loopback listener and pins the destination's
// stable ID before asking the broker for the forward, as StartBrowserView does.
func (v *Vault) StartBrowserView(ctx context.Context, spec BrowserViewSpec) (BrowserViewInfo, error) {
	if spec.Node == "" {
		return BrowserViewInfo{}, vaultNotSent(errors.New("browser view requires a target machine"))
	}
	if spec.Port == 0 {
		spec.Port = 4848
	}
	if spec.Port < 1 || spec.Port > 65535 {
		return BrowserViewInfo{}, vaultNotSent(errors.New("browser dashboard port must be 1..65535"))
	}
	if spec.Listen == "" {
		spec.Listen = "127.0.0.1:0"
	}
	host, port, err := net.SplitHostPort(spec.Listen)
	ip := net.ParseIP(host)
	p, portErr := strconv.Atoi(port)
	if err != nil || ip == nil || !ip.IsLoopback() || portErr != nil || p < 0 || p > 65535 {
		return BrowserViewInfo{}, vaultNotSent(errors.New("browser view listen must be a loopback IP and port"))
	}
	id, err := v.machineID(ctx, spec.Node)
	if err != nil {
		return BrowserViewInfo{}, err
	}
	info, err := v.StartForward(ctx, ForwardSpec{Node: id, Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(spec.Port)), Listen: spec.Listen})
	if err != nil {
		return BrowserViewInfo{}, err
	}
	return BrowserViewInfo{ForwardInfo: info, URL: "http://" + info.Listen}, nil
}

// machineID resolves an exact name or ID to one stable machine ID.
func (v *Vault) machineID(ctx context.Context, node string) (string, error) {
	data, err := v.Machines(ctx, MachineQuery{Node: node, Compact: true})
	if err != nil {
		return "", err
	}
	var machines []machineSummary
	if err := json.Unmarshal(data, &machines); err != nil {
		return "", fmt.Errorf("decode vault machine list: %w", err)
	}
	if len(machines) != 1 || machines[0].ID == "" {
		return "", vaultNotSent(fmt.Errorf("machine %q did not resolve to exactly one stable ID", node))
	}
	return machines[0].ID, nil
}

// Runbooks discovers runbooks as Client.Runbooks does. Project and global
// stores come only from this frontend process's own host and working
// directory, never from the broker, which excludes its host's stores. Worker
// stores are read through the broker and validated here, so a worker cannot
// impersonate a local origin.
func (v *Vault) Runbooks(ctx context.Context, q runbooks.Query) (runbooks.Catalog, error) {
	// Local stores are read only under a live grant, so a pending or denied
	// connection learns nothing about this host's files.
	if checked, _ := ctx.Value(vaultCheckedKey{}).(bool); !checked {
		if err := v.Check(ctx); err != nil {
			return runbooks.Catalog{}, err
		}
	}
	if err := q.Validate(); err != nil {
		return runbooks.Empty(runbooks.Selector{}), err
	}
	if q.Source != "" && q.Source != "project" && q.Source != "global" && q.Source != "worker-user" && q.Source != "worker-common" {
		return runbooks.Catalog{}, errors.New("unknown runbook source")
	}
	workerSource := strings.HasPrefix(q.Source, "worker-")
	if workerSource && (q.Node == "" || q.Local) {
		return runbooks.Catalog{}, errors.New("worker runbook sources require node and cannot be read with local: true")
	}
	local := Client{}
	selector := runbooks.Selector{Node: q.Node}
	if q.Local || q.Node == "" || q.Source == "project" || q.Source == "global" {
		return local.localRunbooks(ctx, selector, q)
	}
	var description map[string]json.RawMessage
	if err := v.Call(ctx, q.Node, "node.describe", map[string]any{}, &description); err != nil {
		if q.File != "" || VaultAuthorizationError(err) {
			return runbooks.Empty(selector), err
		}
		catalog, localErr := runbooks.Empty(selector), error(nil)
		if !workerSource {
			catalog, localErr = local.localRunbooks(ctx, selector, q)
		}
		catalog.Sources = append(catalog.Sources, runbooks.Source{Name: "worker", Error: err.Error()})
		return catalog, localErr
	}
	selector = runbooks.Selector{}
	_ = json.Unmarshal(description["name"], &selector.Node)
	_ = json.Unmarshal(description["id"], &selector.ID)
	if selector.Node == "" {
		selector.Node = q.Node
	}
	catalog := runbooks.Empty(selector)
	if !workerSource {
		var err error
		if catalog, err = local.localRunbooks(ctx, selector, q); err != nil {
			return catalog, err
		}
	}
	workerFailure := func(err error) (runbooks.Catalog, error) {
		if q.File != "" {
			return catalog, err
		}
		catalog.Sources = append(catalog.Sources, runbooks.Source{Name: "worker", Error: err.Error()})
		return catalog, nil
	}
	// The broker's node.describe replaces runbooks with its enriched catalog
	// and keeps the worker's advertisement in runbooksProtocol.
	advertisement := description["runbooksProtocol"]
	if len(advertisement) == 0 {
		advertisement = description["runbooks"]
	}
	var advertised struct {
		Protocol string `json:"protocol"`
	}
	_ = json.Unmarshal(advertisement, &advertised)
	if advertised.Protocol != runbooks.Protocol {
		return workerFailure(errors.New("worker does not advertise runbooks-v1; upgrade it for worker-store discovery"))
	}
	method, params := "runbooks.list", map[string]any{}
	if q.File != "" {
		method = "runbooks.read"
		params["file"] = q.File
	}
	if q.Source != "" {
		params["source"] = q.Source
	}
	var remote runbooks.Catalog
	if err := v.Call(ctx, q.Node, method, params, &remote); err != nil {
		if VaultAuthorizationError(err) {
			return runbooks.Catalog{}, err
		}
		return workerFailure(err)
	}
	if err := validWorkerRunbooks(remote, selector, q); err != nil {
		return workerFailure(err)
	}
	if remote.Node != selector.Node {
		selector.Node = remote.Node
		catalog.Selector = selector
		if !workerSource {
			var err error
			if catalog, err = local.localRunbooks(ctx, selector, q); err != nil {
				return catalog, err
			}
		}
	}
	for _, book := range remote.Runbooks {
		if !runbooks.ValidBook(book, selector) {
			return workerFailure(errors.New("worker returned an invalid runbook binding"))
		}
	}
	catalog.Sources = append(catalog.Sources, remote.Sources...)
	catalog.Runbooks = append(catalog.Runbooks, remote.Runbooks...)
	runbooks.Finalize(&catalog, q.File != "")
	return catalog, nil
}

// validWorkerRunbooks applies the limits and origin checks of
// Client.discoverRunbooks to a worker catalog.
func validWorkerRunbooks(remote runbooks.Catalog, selector runbooks.Selector, q runbooks.Query) error {
	if len(remote.Sources) > 2 || len(remote.Runbooks) > 128 {
		return errors.New("worker runbook catalog exceeds limits")
	}
	if remote.ID != selector.ID || (runbooks.Query{Node: remote.Node}).Validate() != nil || remote.Node == "" {
		return errors.New("worker returned an invalid runbook identity")
	}
	if q.File != "" && (len(remote.Runbooks) != 1 || remote.Runbooks[0].File != q.File || remote.Runbooks[0].Source != q.Source || remote.Runbooks[0].Omitted) {
		return errors.New("worker returned a different or incomplete runbook")
	}
	sources := map[string]bool{}
	for _, source := range remote.Sources {
		if source.Name != "worker-user" && source.Name != "worker-common" || sources[source.Name] || len(source.Directory) > 4096 || len(source.Error) > 4096 || q.Source != "" && source.Name != q.Source {
			return errors.New("worker returned an invalid runbook source")
		}
		sources[source.Name] = true
	}
	for _, book := range remote.Runbooks {
		if !sources[book.Source] || len(book.ID) > 512 || len(book.Title) > 1024 || len(book.Description) > 2048 || len(book.Content) > 1<<20 {
			return errors.New("worker returned an invalid runbook binding")
		}
	}
	return nil
}

// UploadState reads or abandons single-file upload staging on node through
// the broker, using the broker account's owner. The destination is pinned to
// one stable ID first and the reply must name it. Directory transfers keep
// their manifests in the regular client's local state, which the vault
// frontend does not read, so a directory ID reports the worker's answer only.
func (v *Vault) UploadState(ctx context.Context, node, id string, abort bool) (upload.Result, error) {
	var result upload.Result
	if node == "" || !upload.ValidID(id) {
		return result, vaultNotSent(errors.New("upload status/abort requires node and valid id"))
	}
	target, err := v.machineID(ctx, node)
	if err != nil {
		return result, err
	}
	method := upload.StatusMethod
	if abort {
		method = upload.AbortMethod
	}
	if err := v.Call(ctx, target, method, map[string]string{"id": id}, &result); err != nil {
		return result, err
	}
	if result.ID != id || result.NodeID != target {
		return result, errors.New("upload status identity mismatch")
	}
	return result, nil
}

// ReconcileWorkflow inspects a retained workflow and its stable child IDs as
// Client.ReconcileWorkflow does, through the broker. It never resubmits a
// child or restores a grant. Broker errors do not prove that a destination
// replied, so notStarted is never inferred here; absent children are reported
// with their errors instead.
func (v *Vault) ReconcileWorkflow(ctx context.Context, target, id string) (WorkflowReconciliation, error) {
	var report WorkflowReconciliation
	observe, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var parent model.Task
	if err := v.Call(observe, target, "tasks.get", map[string]any{"id": id}, &parent); err != nil {
		return report, err
	}
	report.Parent = model.SummarizeTask(parent)
	if parent.Pruned || parent.Spec.Capability != "workflow.run" {
		return report, errors.New("reconciliation requires a retained workflow.run task")
	}
	var plan struct {
		Steps []model.WorkflowStep `json:"steps"`
	}
	if err := json.Unmarshal(parent.Spec.Args, &plan); err != nil {
		return report, err
	}
	if len(plan.Steps) < 1 || len(plan.Steps) > 100 {
		return report, errors.New("invalid persisted workflow plan")
	}
	report.Steps = make([]WorkflowObservation, len(plan.Steps))
	jobs := make(chan int, len(plan.Steps))
	for i, step := range plan.Steps {
		report.Steps[i] = WorkflowObservation{Name: step.Name, Target: step.Target, ID: step.Task.ID, When: step.When}
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	var denied sync.Once
	var authorization error
	for range min(8, len(plan.Steps)) {
		wg.Go(func() {
			for i := range jobs {
				observation := &report.Steps[i]
				if observation.ID == "" {
					observation.Error = "plan has no stable child ID; consult the workflow log"
					continue
				}
				poll, done := context.WithTimeout(observe, 6*time.Second)
				var task model.Task
				err := poll.Err()
				if err == nil {
					err = v.Call(poll, observation.Target, "tasks.get", map[string]any{"id": observation.ID}, &task)
				}
				done()
				if err != nil {
					if VaultAuthorizationError(err) {
						denied.Do(func() { authorization = err })
					}
					rpc := model.OperationError(err, "unknown")
					observation.Error, observation.Code, observation.Outcome = rpc.Message, rpc.Code, rpc.Outcome
					if len(observation.Error) > 1024 {
						observation.Error = strings.Clone(observation.Error[:1024])
						observation.ErrorTruncated = true
					}
					continue
				}
				summary := model.SummarizeTask(task)
				observation.Task = &summary
			}
		})
	}
	wg.Wait()
	if authorization != nil {
		// A refused or lost grant is the answer, not a per-step detail.
		return report, authorization
	}
	report.Complete = true
	for _, observation := range report.Steps {
		if observation.Task == nil || observation.Task.State != "succeeded" {
			report.Complete = false
		}
	}
	return report, ctx.Err()
}
