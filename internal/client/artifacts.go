package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/koltyakov/control/internal/model"
)

// ArtifactQuery selects one newest-first page of a node's visible artifacts.
type ArtifactQuery struct {
	Node   string `json:"node"`
	Cursor string `json:"cursor,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// Artifacts returns compact pages. Older workers ignore paging and return the
// complete list, which is paged here with the same ordering and cursor rules.
func (c Client) Artifacts(ctx context.Context, q ArtifactQuery) (model.ArtifactPage, error) {
	if q.Node == "" {
		return model.ArtifactPage{}, errors.New("artifact listing requires a node")
	}
	limit := q.Limit
	if limit == 0 {
		limit = model.DefaultArtifactPage
	}
	params := map[string]any{"limit": limit}
	if q.Cursor != "" {
		params["cursor"] = q.Cursor
	}
	var raw json.RawMessage
	if err := c.Call(ctx, q.Node, "artifacts.list", params, &raw); err != nil {
		return model.ArtifactPage{}, err
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || trimmed[0] != '[' {
		var page model.ArtifactPage
		err := json.Unmarshal(raw, &page)
		return page, err
	}
	var artifacts []model.Artifact
	if err := json.Unmarshal(raw, &artifacts); err != nil {
		return model.ArtifactPage{}, err
	}
	node := q.Node
	if len(artifacts) > 0 && artifacts[0].Node != "" {
		node = artifacts[0].Node
	}
	return model.PageArtifacts(node, artifacts, q.Cursor, limit)
}
