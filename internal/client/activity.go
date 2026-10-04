package client

import (
	"context"

	"github.com/koltyakov/control/internal/model"
)

func (c Client) Activities(ctx context.Context, query model.PoolActivityQuery) (model.PoolActivitySnapshot, error) {
	var snapshot model.PoolActivitySnapshot
	err := c.Call(ctx, "", "activities.pool", query, &snapshot)
	return snapshot, err
}
