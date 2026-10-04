package client

import (
	"context"
	"github.com/koltyakov/control/internal/enrollment"
)

func (c Admin) Invite(ctx context.Context, q enrollment.Request) (enrollment.Link, error) {
	if q.Gateway == "" {
		q.Gateway = c.URL
	}
	var link enrollment.Link
	err := c.JSON(ctx, "POST", "/v1/fleet/installations", q, &link)
	return link, err
}
