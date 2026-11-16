package client

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/model"
)

// ErrProxyUnsupported reports a gateway without proxied invitation support.
var ErrProxyUnsupported = errors.New("gateway does not support proxied invitations; upgrade the gateway first")

// SupportsGatewayProxy reads the authenticated feature metadata.
func (c Admin) SupportsGatewayProxy(ctx context.Context) (bool, error) {
	var auth struct {
		GatewayProxy bool `json:"gatewayProxy"`
	}
	if err := c.JSON(ctx, http.MethodGet, "/v1/auth", nil, &auth); err != nil {
		return false, err
	}
	return auth.GatewayProxy, nil
}

// ProxyWorkers lists this fleet's workers that advertise gateway proxy
// endpoints, with the gateway's readiness projection. It needs an owner key.
func (c Admin) ProxyWorkers(ctx context.Context) ([]model.ProxyWorker, error) {
	var status model.PoolActivitySnapshot
	if err := c.JSON(ctx, http.MethodGet, "/v1/status", nil, &status); err != nil {
		return nil, err
	}
	workers := []model.ProxyWorker{}
	for _, n := range status.Nodes {
		if len(n.ProxyEndpoints) == 0 {
			continue
		}
		workers = append(workers, model.ProxyWorker{ID: n.ID, Name: n.Name, Online: n.Online, Disabled: n.Disabled, Endpoints: n.ProxyEndpoints})
	}
	return workers, nil
}

// SelectProxy resolves a worker name or stable ID and an optional endpoint for
// a noninteractive request. It fails clearly when the address is ambiguous;
// the gateway repeats every check when the invitation is created.
func SelectProxy(workers []model.ProxyWorker, worker, endpoint string) (model.ProxyWorker, string, error) {
	var selected *model.ProxyWorker
	for i := range workers {
		if workers[i].ID == worker || workers[i].Name == worker {
			selected = &workers[i]
			break
		}
	}
	if selected == nil {
		return model.ProxyWorker{}, "", fmt.Errorf("machine %q advertises no gateway proxy endpoints; configure proxyEndpoints on it", worker)
	}
	if endpoint != "" {
		normalized, err := model.NormalizeProxyURL(endpoint)
		if err != nil {
			return model.ProxyWorker{}, "", err
		}
		for _, e := range selected.Endpoints {
			if e.URL == normalized {
				return *selected, normalized, nil
			}
		}
		return model.ProxyWorker{}, "", fmt.Errorf("%s does not advertise %s; advertised: %s", selected.Name, normalized, endpointList(selected.Endpoints))
	}
	if len(selected.Endpoints) != 1 {
		return model.ProxyWorker{}, "", fmt.Errorf("%s advertises several proxy endpoints; select one with --proxy-url: %s", selected.Name, endpointList(selected.Endpoints))
	}
	return *selected, selected.Endpoints[0].URL, nil
}

func endpointList(endpoints []model.ProxyEndpointStatus) string {
	parts := make([]string, 0, len(endpoints))
	for _, e := range endpoints {
		label := e.URL
		if e.Interface != "" {
			label += " (" + e.Interface + ")"
		}
		parts = append(parts, label+" "+e.State)
	}
	return strings.Join(parts, ", ")
}

// checkProxySupport runs before submitting a proxied invitation.
func (c Admin) checkProxySupport(ctx context.Context, q enrollment.Request) error {
	if q.ProxyWorker == "" {
		if q.ProxyURL != "" {
			return errors.New("a proxy endpoint requires a proxy worker")
		}
		return nil
	}
	supported, err := c.SupportsGatewayProxy(ctx)
	if err != nil {
		return err
	}
	if !supported {
		return ErrProxyUnsupported
	}
	return nil
}

// confirmProxy checks that the gateway echoed the requested selection.
func confirmProxy(q enrollment.Request, link enrollment.Link) error {
	if q.ProxyWorker == "" {
		if link.GatewayProxy != nil {
			return errors.New("gateway returned an unrequested proxy selection; check control machines invites before creating another invitation")
		}
		return nil
	}
	want := q.ProxyURL
	if want != "" {
		want, _ = model.NormalizeProxyURL(want)
	}
	if link.GatewayProxy == nil || link.GatewayProxy.Worker != q.ProxyWorker || (want != "" && link.GatewayProxy.URL != want) {
		return errors.New("gateway did not confirm the requested proxy selection; upgrade the gateway and check control machines invites before creating another invitation")
	}
	return nil
}
