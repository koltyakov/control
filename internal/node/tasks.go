package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var safeID = model.TaskIDPattern

// maxTaskWait bounds one blocking tasks.get. Callers loop for longer waits.
const maxTaskWait = time.Minute

func (n *Node) taskPath(id string) string {
	return filepath.Join(n.Config.DataDir, "tasks", id+".json")
}
func (n *Node) logPath(id string) string { return filepath.Join(n.Config.DataDir, "tasks", id+".log") }

// taskNotifier wakes log followers on output and state changes, and waiters on
// completion, without taking the node mutex for every log write.
type taskNotifier struct {
	mu      sync.Mutex
	changed chan struct{}
	done    chan struct{}
	// watched records that someone took changed since it was last closed, so
	// log writes with no follower do not allocate a channel each.
	watched bool
	// live is the running provider's log writer, whose retained tail
	// snapshot reads use until it is persisted at the end of execution.
	live atomic.Pointer[limitedLog]
}

func newTaskNotifier() *taskNotifier {
	return &taskNotifier{changed: make(chan struct{}), done: make(chan struct{})}
}

func (t *taskNotifier) next() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.watched = true
	return t.changed
}

func (t *taskNotifier) notify(final bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	select {
	case <-t.done:
		return
	default:
	}
	if final {
		close(t.changed)
		close(t.done)
		return
	}
	if !t.watched {
		return
	}
	close(t.changed)
	t.changed, t.watched = make(chan struct{}), false
}

// acceptance reserves a task ID while its record is written outside n.mu.
type acceptance struct {
	done    chan struct{}
	leaseID string
}

func (n *Node) loadTasks() error {
	dir := filepath.Join(n.Config.DataDir, "tasks")
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	taskPaths := make(map[string]string)
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var task model.Task
		if err = store.Read(path, &task); err != nil {
			return fmt.Errorf("load task %s: %w", path, err)
		}
		if !safeID.MatchString(task.ID) {
			return errors.New("invalid persisted task ID")
		}
		if err := n.reserveTaskIDLocked(task.ID); err != nil {
			return fmt.Errorf("load task %s; preserve task records and tombstones and recover conflicting IDs before starting: %w", path, err)
		}
		n.tasks[task.ID] = &task
		if task.Terminal() {
			n.evictTaskLocked(&task, path, specDigest(task.Spec))
		} else {
			taskPaths[task.ID] = path
		}
	}
	if err := n.loadTombstones(); err != nil {
		return err
	}
	// Validate the complete retained ID set before rewriting interrupted tasks
	// or allowing cleanup to touch paths belonging to a conflicting spelling.
	for _, task := range n.tasks {
		if !task.Terminal() {
			task.State, task.Error, task.Updated = "interrupted", "node restarted before completion; inspect external effects before retrying", time.Now().UTC()
			if err = store.Write(taskPaths[task.ID], task); err != nil {
				return err
			}
			n.evictTaskLocked(task, taskPaths[task.ID], specDigest(task.Spec))
		}
	}
	n.rebuildRecentTasksLocked()
	return nil
}

func persistedTaskError(message string) string {
	const limit = 4096
	// JSON must round-trip the same text for tombstone checksums to match.
	message = strings.ToValidUTF8(message, "\uFFFD")
	if len(message) <= limit {
		return message
	}
	const suffix = " [truncated]"
	end := limit - len(suffix)
	for !utf8.RuneStart(message[end]) {
		end--
	}
	return message[:end] + suffix
}

// Caller holds mu or is loading state before Start. IDs remain case-sensitive
// publicly, but their metadata, logs and workspaces must not alias on disk.
func (n *Node) reserveTaskIDLocked(id string) error {
	key := strings.ToLower(id) // safeID restricts IDs to ASCII.
	if prior := n.taskIDs[key]; prior != "" && prior != id {
		return errors.New("task ID has a case-fold conflict with a retained or pending task ID")
	}
	n.taskIDs[key] = id
	return nil
}

func (n *Node) releaseTaskIDLocked(id string) {
	if n.tasks[id] != nil || n.accepting[id] != nil {
		return
	}
	if _, retained := n.tombstones[id]; !retained && n.taskIDs[strings.ToLower(id)] == id {
		delete(n.taskIDs, strings.ToLower(id))
	}
}

// cloneTask copies a task so callers never share mutable slices with n.tasks.
func cloneTask(task *model.Task) model.Task {
	t := *task
	t.Spec.Args = bytes.Clone(task.Spec.Args)
	t.Spec.Inputs = slices.Clone(task.Spec.Inputs)
	t.Spec.Outputs = slices.Clone(task.Spec.Outputs)
	t.Result = bytes.Clone(task.Result)
	if task.ResultArtifact != nil {
		a := *task.ResultArtifact
		t.ResultArtifact = &a
	}
	t.Artifacts = slices.Clone(task.Artifacts)
	return t
}

func specDigest(spec model.TaskSpec) string {
	sum := sha256.Sum256(model.JSON(spec))
	return hex.EncodeToString(sum[:])
}

// Called with n.mu held. Reports a previous submission of this ID, including a
// pruned one, so a retry never executes the same task twice.
func (n *Node) submittedLocked(id, owner, digest string) (taskPayload, bool, error) {
	if old := n.tasks[id]; old != nil {
		if old.Owner != owner || n.taskDigestLocked(old) != digest {
			return taskPayload{}, true, errors.New("task ID already used with a different owner or specification")
		}
		return n.payloadLocked(old), true, nil
	}
	if pruned, ok := n.tombstones[id]; ok {
		if pruned.Owner != owner || pruned.Spec != digest {
			return taskPayload{}, true, errors.New("task ID already used with a different owner or specification")
		}
		return taskPayload{task: pruned.task()}, true, nil
	}
	return taskPayload{}, false, nil
}

func (n *Node) startTaskContext(parent context.Context, owner string, spec model.TaskSpec) (model.Task, error) {
	status := "rejected"
	defer func() { recordSubmission(parent, spec.ID, status) }()
	if n.ctx == nil {
		return model.Task{}, errors.New("node is not running")
	}
	finishAuthority := func() {}
	retainAuthority := false
	if a, ok := parent.Value(authorityContextKey{}).(authority); ok && a.grant != nil {
		s := a.grant
		n.delegationMu.Lock()
		if s.ctx.Err() != nil || !n.delegationLiveLocked(s, time.Now()) {
			n.delegationMu.Unlock()
			return model.Task{}, errors.New("delegation was revoked or expired")
		}
		s.activeTasks++
		n.delegationMu.Unlock()
		finishAuthority = func() { n.delegationMu.Lock(); s.activeTasks--; s.lastActivity = time.Now(); n.delegationMu.Unlock() }
	}
	defer func() {
		if !retainAuthority {
			finishAuthority()
		}
	}()
	if spec.ID == "" {
		spec.ID = identity.NewID()
	}
	if !safeID.MatchString(spec.ID) {
		return model.Task{}, errors.New("invalid task ID")
	}
	if len(spec.Args) == 0 {
		spec.Args = model.JSON(map[string]any{})
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(spec.Args))
	decoder.UseNumber()
	if err := decoder.Decode(&normalized); err != nil {
		return model.Task{}, err
	}
	spec.Args = model.JSON(normalized)
	if spec.TimeoutSeconds == 0 {
		spec.TimeoutSeconds = 3600
	}
	// One encoding of the final specification serves the size bound and the
	// idempotency digest.
	encoded := model.JSON(spec)
	if len(spec.Args) > 1<<20 || len(spec.Inputs) > 128 || len(spec.Outputs) > 128 || len(encoded) > 2<<20 {
		return model.Task{}, errors.New("task specification exceeds limits: 1 MiB arguments, 128 inputs/outputs, 2 MiB total; use artifacts for bulk data")
	}
	if spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 86400 {
		return model.Task{}, errors.New("task timeout must be 1..86400 seconds")
	}
	sum := sha256.Sum256(encoded)
	digest := hex.EncodeToString(sum[:])
	// Durable submissions remain reconcilable while a machine is disabled. A
	// concurrent submission of the same ID waits for the first to settle.
	reservation := &acceptance{done: make(chan struct{}), leaseID: spec.LeaseID}
	for {
		n.mu.Lock()
		if err := n.reserveTaskIDLocked(spec.ID); err != nil {
			n.mu.Unlock()
			return model.Task{}, err
		}
		if task, found, err := n.submittedLocked(spec.ID, owner, digest); found {
			n.mu.Unlock()
			if err == nil {
				status = "existing"
				return task.load()
			}
			return model.Task{}, err
		}
		pending := n.accepting[spec.ID]
		if pending == nil {
			if _, ok := n.providers[spec.Capability]; !ok {
				n.releaseTaskIDLocked(spec.ID)
				n.mu.Unlock()
				return model.Task{}, fmt.Errorf("unknown capability %q", spec.Capability)
			}
			n.accepting[spec.ID] = reservation
			n.mu.Unlock()
			break
		}
		n.mu.Unlock()
		select {
		case <-pending.done:
		case <-parent.Done():
			return model.Task{}, parent.Err()
		}
	}
	defer func() {
		n.mu.Lock()
		delete(n.accepting, spec.ID)
		n.releaseTaskIDLocked(spec.ID)
		n.mu.Unlock()
		close(reservation.done)
	}()
	// Duplicates of accepted IDs returned above; only new work checks the
	// reserve, before anything is written. The cached probe never holds n.mu.
	if err := n.admitTaskDisk(parent, "task"); err != nil {
		return model.Task{}, err
	}
	// Admission waits for both the request and the node. The returned ticket
	// does not depend on either context and moves to the accepted task.
	admission, stopAdmission := context.WithCancel(parent)
	stopNode := context.AfterFunc(n.ctx, stopAdmission)
	release, err := n.work.Enter(admission)
	stopNode()
	stopAdmission()
	if err != nil {
		return model.Task{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			release()
		}
	}()
	if err := n.lockLeaseSettled(parent); err != nil {
		return model.Task{}, err
	}
	if n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy()) {
		if spec.LeaseID != n.lease.ID || owner != n.lease.Owner || time.Now().After(n.lease.Expires) {
			n.mu.Unlock()
			return model.Task{}, errors.New("node has an exclusive lease; provide the current owner's unexpired leaseId")
		}
	} else if spec.LeaseID != "" {
		n.mu.Unlock()
		return model.Task{}, errors.New("lease expired or does not exist")
	}
	// Reservations count toward the queue bound while their records are written.
	if len(n.cancels)+len(n.accepting) > 256 {
		n.mu.Unlock()
		return model.Task{}, errors.New("node task queue is full")
	}
	if n.ctx == nil || n.ctx.Err() != nil {
		n.mu.Unlock()
		return model.Task{}, errors.New("node is not running")
	}
	n.mu.Unlock()
	now := time.Now().UTC()
	_, phase := n.providerSlots(spec.Capability)
	if len(spec.Inputs) != 0 {
		phase = "waiting for input staging slot"
	}
	task := &model.Task{ID: spec.ID, Owner: owner, Spec: spec, State: "queued", Phase: phase, Created: now, Updated: now}
	// The record is durable before acceptance is visible or acknowledged. The
	// reservation keeps duplicate submissions and lease changes ordered.
	if err := store.Write(n.taskPath(task.ID), task); err != nil {
		// A failed directory sync can leave the renamed record visible.
		// Acceptance was not acknowledged, so a restart must not recover it
		// as an interrupted task or block resubmission of its ID.
		_ = os.Remove(n.taskPath(task.ID))
		return model.Task{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	// Close marks the node closed under n.mu before waiting for owned work, so
	// this check also orders the WaitGroup increment below before that wait.
	if n.closed || n.ctx.Err() != nil {
		// Acceptance was never acknowledged; do not leave a record to recover.
		_ = os.Remove(n.taskPath(task.ID))
		return model.Task{}, errors.New("node is not running")
	}
	n.tasks[task.ID] = task
	notifier := newTaskNotifier()
	n.notifiers[task.ID] = notifier
	ctx, cancel := context.WithTimeout(n.ctx, time.Duration(spec.TimeoutSeconds)*time.Second)
	ctx = model.WithDelegations(ctx, model.Delegations(parent))
	var stopAuthority func() bool
	if a, ok := parent.Value(authorityContextKey{}).(authority); ok {
		ctx = context.WithValue(ctx, authorityContextKey{}, a)
		if a.grant != nil {
			// Do not take delegationMu while holding the task mutex. Admission and
			// cancellation are serialized by the grant lifetime, not peer EOF.
			stopAuthority = context.AfterFunc(a.grant.ctx, cancel)
			if a.grant.ctx.Err() != nil {
				cancel()
			}
		}
	}
	ctx = context.WithValue(ctx, workContextKey{}, n)
	n.cancels[task.ID] = cancel
	n.wg.Add(1)
	accepted = true
	status = "accepted"
	retainAuthority = true
	go func() {
		defer n.wg.Done()
		defer cancel()
		defer release()
		if stopAuthority != nil {
			defer stopAuthority()
		}
		defer finishAuthority()
		n.runTask(ctx, task.ID, notifier)
	}()
	return cloneTask(task), nil
}

// updateTask writes a changed copy of a task outside n.mu, then publishes it.
// Only the task's own goroutine changes its state, so updates cannot interleave.
func (n *Node) updateTask(id string, change func(*model.Task)) (model.Task, error) {
	n.mu.Lock()
	next := cloneTask(n.tasks[id])
	n.mu.Unlock()
	change(&next)
	err := store.Write(n.taskPath(id), next)
	var digest string
	if err == nil && next.Terminal() {
		digest = specDigest(next.Spec)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		return next, err
	}
	*n.tasks[id] = next
	if digest != "" {
		n.evictTaskLocked(n.tasks[id], n.taskPath(id), digest)
	}
	n.recordRecentTaskLocked(&next)
	return next, nil
}

func (n *Node) runTask(ctx context.Context, id string, notifier *taskNotifier) {
	n.mu.Lock()
	task := cloneTask(n.tasks[id])
	n.mu.Unlock()
	ctx = context.WithValue(ctx, activityContextKey{}, activityContext{owner: task.Owner, task: id})
	dir := filepath.Join(n.Config.WorkDir, "tasks", id)
	var result any
	err := n.stageTaskInputs(ctx, id, dir, task.Spec.Inputs)
	if err == nil {
		result, err = n.runTaskProvider(ctx, task, dir, notifier)
	}
	// Provider admission has ended. Publication and its waits must not retain
	// execution, I/O or desktop slots. The accepted task still owns work/grants.
	n.finishTask(id, ctx, result, nil, err)
}

func (n *Node) finishTask(id string, ctx context.Context, result any, artifacts []model.Artifact, err error) {
	// Published outputs and result artifacts stay pinned until the terminal
	// record below references them in n.tasks, on both the durable and the
	// persistence-failure path. Deferred release runs after that publication.
	var pins []func()
	defer func() {
		for _, unpin := range pins {
			unpin()
		}
	}()
	n.mu.Lock()
	outputs := n.tasks[id].Spec.Outputs
	n.mu.Unlock()
	var resultJSON json.RawMessage
	if result != nil {
		if captured, ok := result.(json.RawMessage); ok {
			resultJSON = captured
		} else {
			resultJSON = model.JSON(result)
		}
	}
	largeResult := len(resultJSON) > 1<<20
	if largeResult || (err == nil && len(outputs) != 0) {
		n.taskPhase(id, "waiting for output publication slot")
		release, admissionErr := acquireProviderSlot(ctx, n.publicationPool)
		if admissionErr != nil {
			err = errors.Join(err, admissionErr)
			// Retain inline diagnostics even if output publication is cancelled.
			// Large results need artifact I/O and cannot bypass admission.
			if largeResult {
				resultJSON = nil
			}
		} else {
			defer release()
			n.taskPhase(id, "publishing outputs")
			if err == nil {
				published, publishPins, publishErr := n.publishTaskOutputs(ctx, id, outputs)
				artifacts = append(artifacts, published...)
				pins = append(pins, publishPins...)
				err = publishErr
			}
		}
	}
	var resultArtifact *model.Artifact
	if len(resultJSON) > 1<<20 {
		a, unpin, e := n.importArtifactPinned(ctx, bytes.NewReader(resultJSON), "result.json")
		if e != nil {
			err = errors.Join(err, fmt.Errorf("store task result: %w", e))
		} else {
			resultArtifact = &a
			pins = append(pins, unpin)
		}
		resultJSON = nil
	}
	final := func(task *model.Task) {
		task.Phase = ""
		task.State, task.Updated, task.Artifacts = "succeeded", time.Now().UTC(), artifacts
		task.Result, task.ResultArtifact = resultJSON, resultArtifact
		if err != nil {
			task.State, task.Error = "failed", persistedTaskError(err.Error())
		}
		if ctx.Err() != nil {
			task.State, task.Error = "cancelled", ctx.Err().Error()
		}
		if n.ctx.Err() != nil {
			task.State, task.Error = "interrupted", "node stopped during execution"
		}
	}
	task, writeErr := n.updateTask(id, final)
	n.mu.Lock()
	if writeErr != nil {
		// Report the failure without claiming a durable outcome.
		stored := n.tasks[id]
		final(stored)
		stored.State, stored.Error = "failed", persistedTaskError("persist task completion: "+writeErr.Error())
		task = *stored
		n.recordRecentTaskLocked(stored)
	}
	delete(n.cancels, id)
	notifier := n.notifiers[id]
	delete(n.notifiers, id)
	n.mu.Unlock()
	if notifier != nil {
		notifier.notify(true)
	}
	if task.Spec.Capability == "exec.run" || task.Spec.Capability == "agent.run" || time.Since(task.Created) > time.Second {
		n.system.Request()
	}
}

func (n *Node) taskMethod(ctx context.Context, owner, method string, args json.RawMessage) (any, error) {
	var query struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
		// WaitSeconds makes tasks.get block until the task is terminal or the
		// wait elapses. Older nodes ignore it and answer immediately.
		WaitSeconds int    `json:"waitSeconds"`
		Limit       int    `json:"limit"`
		Cursor      string `json:"cursor"`
		// Tail reads the retained end of a log beyond the head limit.
		// Older nodes ignore it and serve an ordinary head read.
		Tail bool `json:"tail"`
	}
	if err := json.Unmarshal(args, &query); err != nil {
		return nil, err
	}
	if method == "tasks.get" && query.WaitSeconds > 0 {
		return n.waitTask(ctx, owner, query.ID, time.Duration(query.WaitSeconds)*time.Second)
	}
	if method == "tasks.page" {
		return n.taskPage(ctx, owner, query.Cursor, query.Limit)
	}
	n.mu.Lock()
	if method == "tasks.list" {
		tasks := []model.Task{}
		for _, task := range n.tasks {
			if task.Owner == owner || owner == n.Identity.ID {
				if len(tasks) == 100 {
					n.mu.Unlock()
					return nil, errors.New("task listing exceeds 100 records; use tasks.page with limit and cursor")
				}
				tasks = append(tasks, model.Task{ID: task.ID, Owner: task.Owner, Spec: model.TaskSpec{ID: task.ID, Capability: task.Spec.Capability}, State: task.State, Phase: task.Phase, Created: task.Created, Updated: task.Updated})
			}
		}
		n.mu.Unlock()
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created.Before(tasks[j].Created) })
		return tasks, nil
	}
	record, err := n.ownedTaskRecordLocked(owner, query.ID)
	if err != nil {
		n.mu.Unlock()
		return nil, err
	}
	terminal := record.Terminal()
	var payload taskPayload
	var live *limitedLog
	if method != "tasks.logs" {
		payload = n.payloadLocked(record)
	} else if notifier := n.notifiers[query.ID]; notifier != nil {
		live = notifier.live.Load()
	}
	if method == "tasks.cancel" {
		if cancel := n.cancels[query.ID]; cancel != nil {
			cancel()
		}
	}
	n.mu.Unlock()
	switch method {
	case "tasks.get", "tasks.cancel":
		return payload.load()
	case "tasks.logs":
		if query.Offset < 0 {
			return nil, errors.New("invalid log offset")
		}
		return n.taskLogSnapshot(query.ID, query.Offset, query.Tail, terminal, live)
	}
	return nil, errors.New("unknown task method")
}

// Pruned tasks remain visible to their owner as terminal tombstones.
func (n *Node) ownedTask(owner, id string) (model.Task, error) {
	n.mu.Lock()
	task, err := n.ownedTaskRecordLocked(owner, id)
	if err != nil {
		n.mu.Unlock()
		return model.Task{}, err
	}
	payload := n.payloadLocked(task)
	n.mu.Unlock()
	return payload.load()
}

// The returned record may alias node state and must only be read under n.mu.
// Check ownership before copying payloads; log readers need only terminal state.
func (n *Node) ownedTaskRecordLocked(owner, id string) (*model.Task, error) {
	task := n.tasks[id]
	if task == nil {
		pruned, ok := n.tombstones[id]
		if !ok {
			return nil, errors.New("task not found")
		}
		record := pruned.task()
		task = &record
	}
	if task.Owner != owner && owner != n.Identity.ID {
		return nil, errors.New("task belongs to another caller")
	}
	return task, nil
}

// waitTask returns when the task is terminal, the wait elapses, or shortly
// before the request deadline so the response can still be delivered.
func (n *Node) waitTask(ctx context.Context, owner, id string, wait time.Duration) (model.Task, error) {
	wait = min(wait, maxTaskWait)
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, time.Until(deadline)-time.Second)
	}
	n.mu.Lock()
	task, err := n.ownedTaskRecordLocked(owner, id)
	notifier := n.notifiers[id]
	if err != nil {
		n.mu.Unlock()
		return model.Task{}, err
	}
	if task.Terminal() || notifier == nil || wait <= 0 {
		payload := n.payloadLocked(task)
		n.mu.Unlock()
		return payload.load()
	}
	n.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-notifier.done:
	case <-timer.C:
	case <-ctx.Done():
	case <-n.ctx.Done():
	}
	return n.ownedTask(owner, id)
}

func (n *Node) WaitTask(ctx context.Context, target, id string) (model.Task, error) {
	return waitTerminal(ctx, func(ctx context.Context, wait int) (model.Task, error) {
		var task model.Task
		err := n.Call(ctx, target, "tasks.get", map[string]any{"id": id, "waitSeconds": wait}, &task)
		return task, err
	}, 100*time.Millisecond)
}

// waitTerminal asks for a blocking tasks.get. A node that predates the wait
// parameter answers at once, so the loop keeps the caller's poll interval.
func waitTerminal(ctx context.Context, get func(context.Context, int) (model.Task, error), interval time.Duration) (model.Task, error) {
	for {
		started := time.Now()
		task, err := get(ctx, int(maxTaskWait/time.Second)/2)
		if err != nil || task.Terminal() {
			return task, err
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

type limitedLog struct {
	mu        sync.Mutex
	w         io.Writer
	remaining int64
	notify    func()
	// tail, when set, retains the last bytes beyond remaining.
	tail *logTail
}

func (w *limitedLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(b)
	var overflow []byte
	if int64(len(b)) > w.remaining {
		b, overflow = b[:w.remaining], b[w.remaining:]
	}
	n, err := w.w.Write(b)
	w.remaining -= int64(n)
	if n > 0 && w.notify != nil {
		w.notify()
	}
	if err != nil {
		return n, err
	}
	if w.tail != nil {
		w.tail.head += int64(n)
		w.tail.add(overflow)
	}
	return original, nil
}
