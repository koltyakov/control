package client

import (
	"context"
	"errors"

	"github.com/koltyakov/control/internal/enrollment"
)

func (c Admin) Invite(ctx context.Context, q enrollment.Request) (enrollment.Link, error) {
	if q.Name == "auto" || q.Name == "" {
		q.Name, q.AutoName = "", true
	}
	if q.Gateway == "" {
		q.Gateway = c.URL
	}
	var link enrollment.Link
	err := c.JSON(ctx, "POST", "/v1/fleet/installations", q, &link)
	return link, err
}

// Invitation reads the authenticated fleet's registry, including redeemed
// invitations whose public installation URLs are no longer available.
func (c Admin) Invitation(ctx context.Context, id string) (enrollment.Invitation, error) {
	var invitations []enrollment.Invitation
	if err := c.JSON(ctx, "GET", "/v1/fleet/installations", nil, &invitations); err != nil {
		return enrollment.Invitation{}, err
	}
	for _, invitation := range invitations {
		if invitation.ID == id {
			return invitation, nil
		}
	}
	return enrollment.Invitation{}, errors.New("invitation not found")
}
