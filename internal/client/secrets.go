package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/secrets"
)

// SecretTarget resolves one explicit fleet machine and checks management access
// before the CLI reads a credential. It never uses a node's local API or tasks.
func (c Admin) SecretTarget(ctx context.Context, name string) (string, secrets.RemoteList, error) {
	if !secrets.SecureURL(c.URL) || c.Key == "" {
		return "", secrets.RemoteList{}, errors.New("remote secrets require a saved fleet account login and HTTPS, or direct loopback development")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var nodes []model.Node
	if err := c.JSON(ctx, http.MethodGet, "/v1/nodes", nil, &nodes); err != nil {
		return "", secrets.RemoteList{}, err
	}
	for _, n := range nodes {
		if n.ID != name && n.Name != name {
			continue
		}
		var result secrets.RemoteList
		err := c.JSON(ctx, http.MethodGet, secretRoute(n.ID, ""), nil, &result)
		if err == nil && !secrets.ValidManagementKey(result.Key) {
			err = errors.New("worker does not support encrypted secret management; update its node")
		}
		return n.ID, result, err
	}
	return "", secrets.RemoteList{}, errors.New("secret target not found in your fleet")
}

func secretRoute(id, name string) string {
	path := "/v1/fleet/nodes/" + url.PathEscape(id) + "/secrets"
	if name != "" {
		path += "/" + url.PathEscape(name)
	}
	return path
}

func (c Admin) SetSecret(ctx context.Context, id, name, value string, public []byte) error {
	if !secrets.SecureURL(c.URL) || c.Key == "" {
		return errors.New("remote secrets require authenticated HTTPS or direct loopback development")
	}
	envelope, err := secrets.Seal(id, name, value, public)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	// There is one HTTP attempt. A lost acknowledgement never replays a write.
	return c.JSON(ctx, http.MethodPut, secretRoute(id, name), envelope, nil)
}

func (c Admin) DeleteSecret(ctx context.Context, id, name string) error {
	if !secrets.SecureURL(c.URL) || c.Key == "" || !secrets.ValidName(name) {
		return errors.New("remote secret deletion requires a valid name and authenticated HTTPS or direct loopback development")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return c.JSON(ctx, http.MethodDelete, secretRoute(id, name), nil, nil)
}
