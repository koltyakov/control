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
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

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
}

func newTaskNotifier() *taskNotifier {
	return &taskNotifier{changed: make(chan struct{}), done: make(chan struct{})}
}

func (t *taskNotifier) next() <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
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
	close(t.changed)
	if final {
		close(t.done)
		return
	}
	t.changed = make(chan struct{})
}

// acceptance reserves a task ID while its record is written outside n.mu.
type acceptance struct {
	done    chan struct{}
	leaseID string
}

func (n *Node) loadTasks() error {
	paths, err := filepath.Glob(filepath.Join(n.Config.DataDir, "tasks", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		var task model.Task
		if err = store.Read(path, &task); err != nil {
			return fmt.Errorf("load task %s: %w", path, err)
		}
		if !safeID.MatchString(task.ID) {
			return errors.New("invalid persisted task ID")
		}
		if !task.Terminal() {
			task.State, task.Error, task.Updated = "interrupted", "node restarted before completion; inspect external effects before retrying", time.Now().UTC()
			if err = store.Write(path, task); err != nil {
				return err
			}
		}
		n.tasks[task.ID] = &task
	}
	return n.loadTombstones()
}

// cloneTask copies a task so callers never share mutable slices with n.tasks.
func cloneTask(task *model.Task) model.Task {
	t := *task
	t.Spec.Args = bytes.Clone(task.Spec.Args)
	t.Spec.Inputs = slices.Clone(task.Spec.Inputs)
	t.Spec.Outputs = slices.Clone(task.Spec.Outputs)
	t.Result = bytes.Clone(task.Result)
	t.Artifacts = slices.Clone(task.Artifacts)
	return t
}

func specDigest(spec model.TaskSpec) string {
	sum := sha256.Sum256(model.JSON(spec))
	return hex.EncodeToString(sum[:])
}

// Called with n.mu held. Reports a previous submission of this ID, including a
// pruned one, so a retry never executes the same task twice.
func (n *Node) submittedLocked(id, owner, digest string) (model.Task, bool, error) {
	if old := n.tasks[id]; old != nil {
		if old.Owner != owner || specDigest(old.Spec) != digest {
			return model.Task{}, true, errors.New("task ID already used with a different owner or specification")
		}
		return cloneTask(old), true, nil
	}
	if pruned, ok := n.tombstones[id]; ok {
		if pruned.Owner != owner || pruned.Spec != digest {
			return model.Task{}, true, errors.New("task ID already used with a different owner or specification")
		}
		return pruned.task(), true, nil
	}
	return model.Task{}, false, nil
}

func (n *Node) startTaskContext(parent context.Context, owner string, spec model.TaskSpec) (model.Task, error) {
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
	if _, ok := n.providers[spec.Capability]; !ok {
		return model.Task{}, fmt.Errorf("unknown capability %q", spec.Capability)
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
	if spec.TimeoutSeconds < 1 || spec.TimeoutSeconds > 86400 {
		return model.Task{}, errors.New("task timeout must be 1..86400 seconds")
	}
	digest := specDigest(spec)
	// Durable submissions remain reconcilable while a machine is disabled. A
	// concurrent submission of the same ID waits for the first to settle.
	reservation := &acceptance{done: make(chan struct{}), leaseID: spec.LeaseID}
	for {
		n.mu.Lock()
		if task, found, err := n.submittedLocked(spec.ID, owner, digest); found {
			n.mu.Unlock()
			return task, err
		}
		pending := n.accepting[spec.ID]
		if pending == nil {
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
		n.mu.Unlock()
		close(reservation.done)
	}()
	release, err := n.work.Enter(n.ctx)
	if err != nil {
		return model.Task{}, err
	}
	accepted := false
	defer func() {
		if !accepted {
			release()
		}
	}()
	n.mu.Lock()
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
	task := &model.Task{ID: spec.ID, Owner: owner, Spec: spec, State: "queued", Phase: "waiting for worker slot", Created: now, Updated: now}
	// The record is durable before acceptance is visible or acknowledged. The
	// reservation keeps duplicate submissions and lease changes ordered.
	if err := store.Write(n.taskPath(task.ID), task); err != nil {
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
	n.mu.Lock()
	defer n.mu.Unlock()
	if err != nil {
		return next, err
	}
	*n.tasks[id] = next
	return next, nil
}

func (n *Node) runTask(ctx context.Context, id string, notifier *taskNotifier) {
	n.mu.Lock()
	isWorkflow := n.tasks[id].Spec.Capability == "workflow.run" || n.tasks[id].Spec.Capability == "peers.call"
	n.mu.Unlock()
	// Coordination must not occupy the only worker slot needed by a local step.
	if !isWorkflow {
		select {
		case n.slots <- struct{}{}:
			defer func() { <-n.slots }()
		case <-ctx.Done():
			n.finishTask(id, ctx, nil, nil, ctx.Err())
			return
		}
	}
	task, err := n.updateTask(id, func(t *model.Task) { t.State, t.Updated = "running", time.Now().UTC() })
	notifier.notify(false)
	spec, owner := task.Spec, task.Owner
	ctx = context.WithValue(ctx, activityContextKey{}, activityContext{owner: owner, task: id})
	n.taskPhase(id, "preparing workspace")
	if err != nil {
		n.finishTask(id, ctx, nil, nil, err)
		return
	}
	dir := filepath.Join(n.Config.WorkDir, "tasks", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		n.finishTask(id, ctx, nil, nil, err)
		return
	}
	log, err := os.OpenFile(n.logPath(id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		n.finishTask(id, ctx, nil, nil, err)
		return
	}
	defer func() { _ = log.Close() }()
	output := &limitedLog{w: log, remaining: 10 << 20, notify: func() { notifier.notify(false) }}
	var result any
	var artifacts []model.Artifact
	err = func() error {
		root, e := os.OpenRoot(dir)
		if e != nil {
			return e
		}
		defer func() { _ = root.Close() }()
		n.taskPhase(id, "fetching inputs")
		for _, input := range spec.Inputs {
			artifact, e := n.pullArtifact(ctx, input.Artifact)
			if e != nil {
				return fmt.Errorf("fetch input: %w", e)
			}
			if !filepath.IsLocal(input.Path) {
				return errors.New("input path must be relative to the task workspace")
			}
			if e = root.MkdirAll(filepath.Dir(input.Path), 0700); e != nil {
				return e
			}
			dest, e := root.OpenFile(input.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
			if e != nil {
				return e
			}
			source, e := os.Open(n.artifactPath(artifact.ID))
			if e != nil {
				_ = dest.Close()
				return e
			}
			// Copy rather than link: providers may modify inputs in place, which
			// must never alter the content-addressed store. File-to-file copies
			// use the kernel's copy path where the platform provides one.
			_, e = io.Copy(dest, source)
			_ = source.Close()
			ce := dest.Close()
			if e != nil {
				return e
			}
			if ce != nil {
				return ce
			}
		}
		n.taskPhase(id, "executing")
		result, e = n.providers[spec.Capability].Run(ctx, spec.Args, Execution{Dir: dir, Log: output, Owner: owner})
		if e != nil {
			return e
		}
		n.taskPhase(id, "publishing outputs")
		for _, path := range spec.Outputs {
			file, e := root.Open(path)
			if e != nil {
				return fmt.Errorf("output %s: %w", path, e)
			}
			artifact, e := n.importArtifact(ctx, file, filepath.Base(path))
			_ = file.Close()
			if e != nil {
				return e
			}
			artifacts = append(artifacts, artifact)
		}
		return nil
	}()
	n.finishTask(id, ctx, result, artifacts, err)
}

func (n *Node) finishTask(id string, ctx context.Context, result any, artifacts []model.Artifact, err error) {
	final := func(task *model.Task) {
		task.Phase = ""
		task.State, task.Updated, task.Artifacts = "succeeded", time.Now().UTC(), artifacts
		if result != nil {
			task.Result = model.JSON(result)
		}
		if err != nil {
			task.State, task.Error = "failed", err.Error()
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
		stored.State, stored.Error = "failed", "persist task completion: "+writeErr.Error()
		task = *stored
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
		WaitSeconds int `json:"waitSeconds"`
	}
	if err := json.Unmarshal(args, &query); err != nil {
		return nil, err
	}
	if method == "tasks.get" && query.WaitSeconds > 0 {
		return n.waitTask(ctx, owner, query.ID, time.Duration(query.WaitSeconds)*time.Second)
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if method == "tasks.list" {
		tasks := []model.Task{}
		for _, task := range n.tasks {
			if task.Owner == owner || owner == n.Identity.ID {
				tasks = append(tasks, cloneTask(task))
			}
		}
		sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created.Before(tasks[j].Created) })
		return tasks, nil
	}
	task, err := n.ownedTaskLocked(owner, query.ID)
	if err != nil {
		return nil, err
	}
	switch method {
	case "tasks.get":
		return task, nil
	case "tasks.cancel":
		if cancel := n.cancels[query.ID]; cancel != nil {
			cancel()
		}
		return task, nil
	case "tasks.logs":
		if query.Offset < 0 {
			return nil, errors.New("invalid log offset")
		}
		f, err := os.Open(n.logPath(query.ID))
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{"text": "", "offset": query.Offset, "terminal": task.Terminal()}, nil
		}
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		if _, err = f.Seek(query.Offset, io.SeekStart); err != nil {
			return nil, err
		}
		b, err := io.ReadAll(io.LimitReader(f, 64*1024))
		return map[string]any{"text": string(b), "offset": query.Offset + int64(len(b)), "terminal": task.Terminal()}, err
	}
	return nil, errors.New("unknown task method")
}

// Called with n.mu held. Pruned tasks remain visible to their owner as
// terminal tombstones.
func (n *Node) ownedTaskLocked(owner, id string) (model.Task, error) {
	var task model.Task
	if stored := n.tasks[id]; stored != nil {
		task = cloneTask(stored)
	} else if pruned, ok := n.tombstones[id]; ok {
		task = pruned.task()
	} else {
		return model.Task{}, errors.New("task not found")
	}
	if task.Owner != owner && owner != n.Identity.ID {
		return model.Task{}, errors.New("task belongs to another caller")
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
	task, err := n.ownedTaskLocked(owner, id)
	notifier := n.notifiers[id]
	n.mu.Unlock()
	if err != nil || task.Terminal() || notifier == nil || wait <= 0 {
		return task, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-notifier.done:
	case <-timer.C:
	case <-ctx.Done():
	case <-n.ctx.Done():
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.ownedTaskLocked(owner, id)
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
}

func (w *limitedLog) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	original := len(b)
	if int64(len(b)) > w.remaining {
		b = b[:w.remaining]
	}
	n, err := w.w.Write(b)
	w.remaining -= int64(n)
	if n > 0 && w.notify != nil {
		w.notify()
	}
	if err != nil {
		return n, err
	}
	return original, nil
}
