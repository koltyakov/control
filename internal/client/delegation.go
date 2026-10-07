package client

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/transport"
)

// Prepare authority at the destinations before instructing the coordinating
// worker. Grants are carried only by that invocation, never its local API.
func prepareDelegations(ctx context.Context, peer *transport.Peer, target, method string, params json.RawMessage) (_ json.RawMessage, grants []model.Delegation, err error) {
	capability, args := method, params
	var task model.TaskSpec
	if method == "tasks.start" {
		if err = json.Unmarshal(params, &task); err != nil {
			return nil, nil, err
		}
		if task.ID == "" {
			task.ID = identity.NewID()
		}
		capability, args = task.Capability, task.Args
	}
	if capability != "peers.call" && capability != "workflow.run" && method != "artifacts.deliver" {
		return params, nil, nil
	}
	coordinator, err := peer.Lookup(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	defer func() {
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			for _, grant := range grants {
				_ = peerCall(cleanup, peer, grant.Target, "access.revoke", map[string]any{"id": grant.ID}, nil)
			}
		}
	}()
	issue := func(destination string, q model.Delegation) error {
		q.Subject = coordinator.ID
		q.Alias = destination
		var grant model.Delegation
		if err := peerCall(ctx, peer, destination, "access.grant", q, &grant); err != nil {
			return err
		}
		grants = append(grants, grant)
		return nil
	}
	switch capability {
	case "peers.call":
		var q struct {
			Target string          `json:"target"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err = json.Unmarshal(args, &q); err != nil {
			return nil, grants, err
		}
		if q.Method == "tasks.start" {
			var child model.TaskSpec
			if err = json.Unmarshal(q.Params, &child); err != nil {
				return nil, grants, err
			}
			if child.ID == "" {
				child.ID = identity.NewID()
				if method == "tasks.start" {
					child.ID = identity.ID([]byte(task.ID + "\x00peer-call"))
				}
			}
			q.Params = model.JSON(child)
		}
		if err = issue(q.Target, model.Delegation{Method: q.Method, Params: q.Params}); err != nil {
			return nil, grants, err
		}
		args = model.JSON(q)
	case "workflow.run":
		var workflow struct {
			Steps       []model.WorkflowStep `json:"steps"`
			MaxParallel int                  `json:"maxParallel,omitempty"`
		}
		if err = json.Unmarshal(args, &workflow); err != nil {
			return nil, grants, err
		}
		if len(workflow.Steps) == 0 || len(workflow.Steps) > 100 {
			return nil, grants, errors.New("workflow requires 1..100 steps")
		}
		byName := map[string]int{}
		for i := range workflow.Steps {
			step := &workflow.Steps[i]
			if _, exists := byName[step.Name]; exists || step.Name == "" {
				return nil, grants, errors.New("workflow step names must be nonempty and unique")
			}
			byName[step.Name] = i
			destination, lookupErr := peer.Lookup(ctx, step.Target)
			if lookupErr != nil {
				return nil, grants, lookupErr
			}
			step.Target = destination.ID
			if step.Task.ID == "" {
				step.Task.ID = identity.NewID()
				if method == "tasks.start" {
					step.Task.ID = identity.ID([]byte(task.ID + "\x00" + step.Name))
				}
			}
		}
		for _, step := range workflow.Steps {
			q := model.Delegation{Method: "tasks.start", Params: model.JSON(step.Task)}
			for _, input := range step.Inputs {
				index, exists := byName[input.Step]
				if !exists || input.Artifact < 0 {
					return nil, grants, errors.New("invalid workflow input")
				}
				q.InputsFrom = append(q.InputsFrom, model.DelegatedInput{Node: workflow.Steps[index].Target, Path: input.Path, TaskID: workflow.Steps[index].Task.ID, Artifact: input.Artifact})
			}
			for _, consumer := range workflow.Steps {
				for _, input := range consumer.Inputs {
					if input.Step == step.Name {
						q.DeliverTo = append(q.DeliverTo, consumer.Target)
					}
				}
			}
			if err = issue(step.Target, q); err != nil {
				return nil, grants, err
			}
		}
		args = model.JSON(workflow)
	}
	if method == "artifacts.deliver" {
		var q struct {
			ID     string `json:"id"`
			Target string `json:"target"`
		}
		if err = json.Unmarshal(params, &q); err != nil {
			return nil, grants, err
		}
		var artifacts []model.Artifact
		if err = peerCall(ctx, peer, target, "artifacts.list", map[string]any{}, &artifacts); err != nil {
			return nil, grants, err
		}
		found := false
		for _, artifact := range artifacts {
			if artifact.ID == q.ID {
				found = true
				err = issue(q.Target, model.Delegation{Method: "artifacts.pull", Params: model.JSON(map[string]any{"artifact": artifact})})
				break
			}
		}
		if !found {
			err = errors.New("artifact not found")
		}
		return params, grants, err
	}
	if method == "tasks.start" {
		task.Args = args
		params = model.JSON(task)
	} else {
		params = args
	}
	return params, grants, nil
}
