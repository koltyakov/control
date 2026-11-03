package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
)

// The caller holds the profile's installation lock. Validate a new invitation
// before atomically replacing pending recovery state or stopping the node.
func prepareEnrollment(ctx context.Context, config, link string, requireAutoName bool) (pendingEnrollment, error) {
	link = strings.TrimRight(link, "/")
	path := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
	var previous pendingEnrollment
	err := store.Read(path, &previous)
	if err != nil && !os.IsNotExist(err) {
		return pendingEnrollment{}, err
	}
	if err == nil && strings.TrimRight(previous.URL, "/") == link {
		if requireAutoName && !previous.Invitation.AutoName {
			return pendingEnrollment{}, errors.New("pending enrollment does not use automatic naming")
		}
		if err = replaceRetiredIdentity(ctx, config, &previous); err != nil {
			return pendingEnrollment{}, err
		}
		return previous, store.Write(path, previous)
	}
	pending, err := inspectInvitation(ctx, config, link, requireAutoName, previous)
	if err != nil {
		return pending, err
	}
	if err = refreshEnrollmentIdentity(ctx, config, &pending); err != nil {
		return pending, err
	}
	return pending, store.Write(path, pending)
}

// inspectInvitation validates a new invitation against this machine and profile
// without writing recovery state or a new identity.
func inspectInvitation(ctx context.Context, config, link string, requireAutoName bool, previous pendingEnrollment) (pendingEnrollment, error) {
	var pending pendingEnrollment
	c := client.Admin{URL: link}
	err := c.JSON(ctx, "GET", "/info?arch="+runtime.GOARCH, nil, &pending.Invitation)
	if err != nil {
		return pending, err
	}
	if requireAutoName && !pending.Invitation.AutoName {
		return pending, errors.New("this installation command requires an automatic-name invitation")
	}
	if !strings.HasPrefix(link, pending.Invitation.Gateway+"/install/") {
		return pending, errors.New("invitation gateway mismatch")
	}
	a := pending.Invitation.Asset
	if a.OS != runtime.GOOS || a.Arch != runtime.GOARCH {
		return pending, errors.New("invitation platform does not match this machine")
	}
	if !update.Matches(buildinfo.Current(), a) {
		return pending, errors.New("enrollment must use the executable pinned by the invitation")
	}
	if previous.URL != "" {
		if previous.Invitation.Gateway != pending.Invitation.Gateway {
			return pending, errors.New("pending enrollment belongs to another gateway; use a separate profile")
		}
		if previous.Invitation.UserID != "" && pending.Invitation.UserID != previous.Invitation.UserID {
			return pending, errors.New("pending enrollment belongs to another fleet; use a separate profile")
		}
		// A lost redemption response may already have enrolled this replacement
		// identity. Reuse it so a new ticket cannot create a second machine.
		pending.IdentityID = previous.IdentityID
		pending.PreviousIdentityID = previous.PreviousIdentityID
		pending.DataDir = previous.DataDir
	}
	hostname := ""
	if pending.Invitation.AutoName {
		hostname, err = os.Hostname()
		if err != nil {
			return pending, fmt.Errorf("read target machine hostname: %w", err)
		}
	}
	pending.Invitation.Name, err = pending.Invitation.ResolveName(hostname)
	if err != nil {
		return pending, err
	}
	pending.Credential, err = randomCredential()
	if err != nil {
		return pending, err
	}
	pending.URL = link
	return pending, prepareReenrollment(config, &pending)
}

// preflightEnrollment finds problems that would otherwise surface only after a
// Hybrid System companion is installed. It writes nothing and keeps the ticket.
func preflightEnrollment(ctx context.Context, config, link, mode string, requireAutoName bool) error {
	path := filepath.Join(filepath.Dir(config), "pending-enrollment.json")
	var previous pendingEnrollment
	err := store.Read(path, &previous)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pending := previous
	if err != nil || strings.TrimRight(previous.URL, "/") != link {
		if pending, err = inspectInvitation(ctx, config, link, requireAutoName, previous); err != nil {
			return err
		}
	} else if requireAutoName && !previous.Invitation.AutoName {
		return errors.New("pending enrollment does not use automatic naming")
	}
	if err = installation.CheckUserStartup(ctx, mode); err != nil {
		return err
	}
	if warning := installation.UserStartupWarning(); warning != "" {
		fmt.Fprintln(os.Stderr, "Warning:", warning)
	}
	cfg, err := node.LoadConfig(config)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	id, err := preflightIdentity(ctx, cfg, pending)
	if err != nil {
		return err
	}
	if err = installation.CheckLocalAPI(ctx, cfg, id); err != nil {
		return err
	}
	return checkRedemption(ctx, link, pending, id)
}

// A retired identity is replaced during real enrollment, so it counts as new.
func preflightIdentity(ctx context.Context, cfg node.Config, pending pendingEnrollment) (*identity.Identity, error) {
	if cfg.DataDir == "" {
		return nil, nil
	}
	dir := cfg.DataDir
	if pending.DataDir != "" {
		dir = pending.DataDir
	}
	id, err := identity.Load(dir)
	if err != nil {
		return nil, err
	}
	state, _, err := (client.Admin{URL: pending.Invitation.Gateway}).InspectMachineState(ctx, id)
	if err != nil {
		return nil, err
	}
	if state.Unregistered {
		if pending.Invitation.ReplaceID != "" {
			return nil, errors.New("invitation reserves a retired identity; create a new invitation")
		}
		return nil, nil
	}
	return id, nil
}

func checkRedemption(ctx context.Context, link string, pending pendingEnrollment, id *identity.Identity) error {
	ticket := link[strings.LastIndex(link, "/")+1:]
	q := enrollment.Redemption{}
	if pending.Invitation.AutoName {
		q.Name = pending.Invitation.Name
	}
	if id != nil {
		q.PublicKey = id.Public
		q.Signature = ed25519.Sign(id.Private, enrollment.CheckMessage(ticket, q))
	}
	err := (client.Admin{URL: link}).JSON(ctx, "POST", "/check", q, nil)
	// Older gateways have no check route; redemption still enforces the rules.
	if err != nil && strings.HasPrefix(err.Error(), "gateway 404 ") {
		return nil
	}
	return err
}
