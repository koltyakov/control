package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var safeID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

func (n *Node) taskPath(id string) string {
	return filepath.Join(n.Config.DataDir, "tasks", id+".json")
}
func (n *Node) logPath(id string) string { return filepath.Join(n.Config.DataDir, "tasks", id+".log") }

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
	return nil
}

func cloneTask(task *model.Task) model.Task {
	var copy model.Task
	_ = json.Unmarshal(model.JSON(task), &copy)
	return copy
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
	// Durable submissions remain reconcilable while a machine is disabled.
	n.mu.Lock()
	if old := n.tasks[spec.ID]; old != nil {
		matches := old.Owner == owner && string(model.JSON(old.Spec)) == string(model.JSON(spec))
		copy := cloneTask(old)
		n.mu.Unlock()
		if !matches {
			return model.Task{}, errors.New("task ID already used with a different owner or specification")
		}
		return copy, nil
	}
	n.mu.Unlock()
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
	defer n.mu.Unlock()
	if old := n.tasks[spec.ID]; old != nil {
		if old.Owner != owner || string(model.JSON(old.Spec)) != string(model.JSON(spec)) {
			return model.Task{}, errors.New("task ID already used with a different owner or specification")
		}
		return cloneTask(old), nil
	}
	if n.lease != nil && (time.Now().Before(n.lease.Expires) || n.leaseBusy()) {
		if spec.LeaseID != n.lease.ID || owner != n.lease.Owner || time.Now().After(n.lease.Expires) {
			return model.Task{}, errors.New("node has an exclusive lease; provide the current owner's unexpired leaseId")
		}
	} else if spec.LeaseID != "" {
		return model.Task{}, errors.New("lease expired or does not exist")
	}
	if len(n.cancels) >= 256 {
		return model.Task{}, errors.New("node task queue is full")
	}
	if n.ctx == nil || n.ctx.Err() != nil {
		return model.Task{}, errors.New("node is not running")
	}
	now := time.Now().UTC()
	task := &model.Task{ID: spec.ID, Owner: owner, Spec: spec, State: "queued", Phase: "waiting for worker slot", Created: now, Updated: now}
	if err := store.Write(n.taskPath(task.ID), task); err != nil {
		return model.Task{}, err
	}
	n.tasks[task.ID] = task
	n.taskChanges[task.ID] = make(chan struct{})
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
		n.runTask(ctx, task.ID)
	}()
	return cloneTask(task), nil
}

func (n *Node) runTask(ctx context.Context, id string) {
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
	n.mu.Lock()
	task := n.tasks[id]
	task.State, task.Updated = "running", time.Now().UTC()
	err := store.Write(n.taskPath(id), task)
	spec, owner := task.Spec, task.Owner
	n.mu.Unlock()
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
	output := &limitedLog{w: log, remaining: 10 << 20, notify: func() { n.mu.Lock(); n.notifyTaskLocked(id); n.mu.Unlock() }}
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
	n.mu.Lock()
	defer n.mu.Unlock()
	task := n.tasks[id]
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
	if e := store.Write(n.taskPath(id), task); e != nil {
		task.State, task.Error = "failed", "persist task completion: "+e.Error()
	}
	delete(n.cancels, id)
	n.notifyTaskLocked(id)
	if task.Spec.Capability == "exec.run" || task.Spec.Capability == "agent.run" || time.Since(task.Created) > time.Second {
		n.system.Request()
	}
}

func (n *Node) taskMethod(owner, method string, args json.RawMessage) (any, error) {
	var query struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
	}
	if err := json.Unmarshal(args, &query); err != nil {
		return nil, err
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
	task := n.tasks[query.ID]
	if task == nil {
		return nil, errors.New("task not found")
	}
	if task.Owner != owner && owner != n.Identity.ID {
		return nil, errors.New("task belongs to another caller")
	}
	switch method {
	case "tasks.get":
		return cloneTask(task), nil
	case "tasks.cancel":
		if cancel := n.cancels[query.ID]; cancel != nil {
			cancel()
		}
		return cloneTask(task), nil
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

func (n *Node) WaitTask(ctx context.Context, target, id string) (model.Task, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var task model.Task
		if err := n.Call(ctx, target, "tasks.get", map[string]any{"id": id}, &task); err != nil {
			return task, err
		}
		if task.Terminal() {
			return task, nil
		}
		select {
		case <-ctx.Done():
			return task, ctx.Err()
		case <-ticker.C:
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
