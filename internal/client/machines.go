package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/runbooks"
)

func (c Admin) ManageMachine(ctx context.Context, id, action string) error {
	path := "/v1/fleet/nodes/" + url.PathEscape(id)
	switch action {
	case "enable", "disable":
		return c.JSON(ctx, http.MethodPatch, path, map[string]bool{"disabled": action == "disable"}, nil)
	case "unregister":
		return c.JSON(ctx, http.MethodDelete, path+"?stop=true", nil, nil)
	default:
		return fmt.Errorf("unknown machine action %q", action)
	}
}

func (c Admin) RenameMachine(ctx context.Context, id, name string) error {
	if err := checkRunbookParents(); err != nil {
		return err
	}
	return c.renameMachine(ctx, id, name, filepath.Join(runbooks.ProjectDirectory, "machines"))
}

// ForceRenameMachine retracts pending invitations for the destination name.
// Registered identities and redeemed enrollments remain protected.
func (c Admin) ForceRenameMachine(ctx context.Context, id, name string) error {
	if err := checkRunbookParents(); err != nil {
		return err
	}
	return c.renameMachineWithForce(ctx, id, name, filepath.Join(runbooks.ProjectDirectory, "machines"), true)
}

func checkRunbookParents() error {
	for _, p := range []string{".control", runbooks.ProjectDirectory} {
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return errors.New("runbook parent must be a directory, not a symlink")
		}
	}
	return nil
}

func (c Admin) ResolveMachine(ctx context.Context, name string) (model.Node, error) {
	var nodes []model.Node
	if err := c.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
		return model.Node{}, err
	}
	for _, n := range nodes {
		if n.ID == name || n.Name == name {
			return n, nil
		}
	}
	return model.Node{}, fmt.Errorf("unknown machine %q", name)
}
