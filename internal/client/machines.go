package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/koltyakov/control/internal/model"
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
