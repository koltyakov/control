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
	if err := c.checkProxySupport(ctx, q); err != nil {
		return enrollment.Link{}, err
	}
	var link enrollment.Link
	err := c.JSON(ctx, "POST", "/v1/fleet/installations", q, &link)
	if err == nil {
		// An uncertain or unconfirmed response never triggers a retry here.
		if proxyErr := confirmProxy(q, link); proxyErr != nil {
			return link, proxyErr
		}
	}
	if err == nil && q.ServiceMode != "" && link.ServiceMode != q.ServiceMode {
		return link, errors.New("gateway did not confirm the requested startup context; upgrade the gateway and check control machines invites before creating another invitation")
	}
	if err == nil && q.Linger && !link.Linger {
		return link, errors.New("gateway did not confirm the requested linger setting; upgrade the gateway and check control machines invites before creating another invitation")
	}
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
