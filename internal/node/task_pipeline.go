package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func (n *Node) stageTaskInputs(ctx context.Context, id, dir string, inputs []model.Input) error {
	if len(inputs) == 0 {
		// A transfer backlog must not stop tasks with no input artifacts.
		return ctx.Err()
	}
	n.taskPhase(id, "waiting for input staging slot")
	release, err := acquireProviderSlot(ctx, n.inputStagingPool)
	if err != nil {
		return err
	}
	defer release()
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	n.taskPhase(id, "fetching inputs")
	for _, input := range inputs {
		if !filepath.IsLocal(input.Path) {
			return errors.New("input path must be relative to the task workspace")
		}
		artifact, err := n.pullArtifact(ctx, input.Artifact)
		if err != nil {
			return fmt.Errorf("fetch input: %w", err)
		}
		if err := root.MkdirAll(filepath.Dir(input.Path), 0700); err != nil {
			return err
		}
		dest, err := root.OpenFile(input.Path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return err
		}
		source, err := os.Open(n.artifactPath(artifact.ID))
		if err != nil {
			_ = dest.Close()
			return err
		}
		// Copy rather than link: a provider may modify its inputs, but never the
		// immutable artifact. Check cancellation while copying large local files.
		_, err = io.Copy(dest, contextReader{ctx, source})
		_ = source.Close()
		closeErr := dest.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return ctx.Err()
}

func (n *Node) runTaskProvider(ctx context.Context, task model.Task, dir string, notifier *taskNotifier) (any, error) {
	pool, phase := n.providerSlots(task.Spec.Capability)
	n.taskPhase(task.ID, phase)
	release, err := acquireProviderSlot(ctx, pool)
	if err != nil {
		return nil, err
	}
	defer release()
	if _, err := n.updateTask(task.ID, func(t *model.Task) { t.State, t.Updated = "running", time.Now().UTC() }); err != nil {
		return nil, err
	}
	notifier.notify(false)
	n.taskPhase(task.ID, "preparing workspace")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(n.logPath(task.ID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = log.Close() }()
	output := &limitedLog{w: log, remaining: 10 << 20, notify: func() { notifier.notify(false) }}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n.taskPhase(task.ID, "executing")
	result, err := n.providers[task.Spec.Capability].Run(ctx, task.Spec.Args, Execution{Dir: dir, Log: output, Owner: task.Owner})
	if result == nil {
		return nil, err
	}
	// Detach result storage before admitting another provider invocation. A
	// provider serialized by a one-slot pool may reuse its maps or buffers.
	// Encoding is bounded by provider admission; artifact I/O happens later.
	return model.JSON(result), err
}

// The caller holds output-publication admission, never a provider slot.
func (n *Node) publishTaskOutputs(ctx context.Context, id string, outputs []string) ([]model.Artifact, error) {
	if len(outputs) == 0 {
		return nil, ctx.Err()
	}
	root, err := os.OpenRoot(filepath.Join(n.Config.WorkDir, "tasks", id))
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	var artifacts []model.Artifact
	for _, path := range outputs {
		if err := ctx.Err(); err != nil {
			return artifacts, err
		}
		file, err := root.Open(path)
		if err != nil {
			return artifacts, fmt.Errorf("output %s: %w", path, err)
		}
		artifact, err := n.importArtifact(ctx, file, filepath.Base(path))
		_ = file.Close()
		if err != nil {
			return artifacts, err
		}
		artifacts = append(artifacts, artifact)
	}
	return artifacts, nil
}
