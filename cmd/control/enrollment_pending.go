package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/client"
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
	var pending pendingEnrollment
	c := client.Admin{URL: link}
	if err = c.JSON(ctx, "GET", "/info?arch="+runtime.GOARCH, nil, &pending.Invitation); err != nil {
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
	if err = prepareReenrollment(config, &pending); err != nil {
		return pending, err
	}
	if err = refreshEnrollmentIdentity(ctx, config, &pending); err != nil {
		return pending, err
	}
	return pending, store.Write(path, pending)
}
