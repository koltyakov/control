package node

import (
	"errors"
	"sync"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

// A durable terminal record is immutable. Readers reserve its file under mu
// before pruning can detach it, then do disk I/O without holding that mutex.
type coldTask struct {
	path, digest string
	readers      sync.WaitGroup
}

type taskPayload struct {
	task model.Task
	cold *coldTask
}

func (n *Node) payloadLocked(t *model.Task) taskPayload {
	if c := n.coldTasks[t.ID]; c != nil {
		c.readers.Add(1)
		return taskPayload{task: model.Task{ID: t.ID, Owner: t.Owner, State: t.State}, cold: c}
	}
	return taskPayload{task: cloneTask(t)}
}

func (p taskPayload) load() (model.Task, error) {
	if p.cold == nil {
		return p.task, nil
	}
	defer p.cold.readers.Done()
	var task model.Task
	if err := store.Read(p.cold.path, &task); err != nil {
		return model.Task{}, err
	}
	if task.ID != p.task.ID || task.Owner != p.task.Owner || task.State != p.task.State {
		return model.Task{}, errors.New("persisted task no longer matches retained metadata")
	}
	return task, nil
}

// Keep artifact IDs in their original positions for retained-reference and
// output-grant checks. No caller receives this reduced record as a full task.
func (n *Node) evictTaskLocked(t *model.Task, path, digest string) {
	t.Result, t.Spec.Args, t.Spec.Outputs = nil, nil, nil
	inputs := make([]model.Input, len(t.Spec.Inputs))
	for i, input := range t.Spec.Inputs {
		inputs[i].Artifact.ID = input.Artifact.ID
	}
	t.Spec.Inputs = inputs
	n.coldTasks[t.ID] = &coldTask{path: path, digest: digest}
}

func (n *Node) taskDigestLocked(t *model.Task) string {
	if c := n.coldTasks[t.ID]; c != nil {
		return c.digest
	}
	return specDigest(t.Spec)
}
