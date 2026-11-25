package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/gatewayroute"
	"github.com/koltyakov/control/internal/update"
)

// Admin calls gateway management and enrollment APIs. Route selects a direct
// connection or the profile's explicit gateway proxy.
type Admin struct {
	URL, Key string
	Route    gatewayroute.Route
}

func (c Admin) request(ctx context.Context, method, path string, body io.Reader, size int64, contentType string, result any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+path, body)
	if err != nil {
		return err
	}
	if size >= 0 {
		req.ContentLength = size
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", contentType)
	resp, err := c.Route.Client(3*time.Minute, false).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if resp.StatusCode == http.StatusConflict && strings.TrimSpace(string(text)) == enrollment.ErrPendingInvitation.Error() {
			return fmt.Errorf("gateway %s: %w", resp.Status, enrollment.ErrPendingInvitation)
		}
		return fmt.Errorf("gateway %s: %s", resp.Status, strings.TrimSpace(string(text)))
	}
	if result != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(result)
	}
	return nil
}

// OwnerRole reports credentials that administer a fleet: account (user) and
// superuser keys.
func OwnerRole(role string) bool { return role == "user" || role == "superuser" }

// ExecutionRole reports credentials whose account-client sessions receive
// execution authority. Both orchestrator roles qualify without fleet
// administration; common and installation keys cannot run work.
func ExecutionRole(role string) bool {
	return OwnerRole(role) || role == "orchestrator" || role == "orchestrator-enroll"
}

func (c Admin) IsSuperuser(ctx context.Context) bool {
	return c.Role(ctx) == "superuser"
}

// Role is a best-effort lookup for help and display paths. It keeps a short
// bound so an offline gateway does not stall usage output.
func (c Admin) Role(ctx context.Context) string {
	role, _, _ := c.account(ctx, roleLookupTimeout)
	return role
}

const (
	roleLookupTimeout = 2 * time.Second
	// accountTimeout bounds explicit commands on a slow first connection.
	// A caller deadline that is shorter still applies.
	accountTimeout = 15 * time.Second
)

// AuthRole preserves authentication and connection errors for explicit commands.
// Role is used by help, where unavailable credentials simply hide management.
func (c Admin) AuthRole(ctx context.Context) (string, error) {
	role, _, err := c.Account(ctx)
	return role, err
}

// Account returns the credential's role and the browser username of a management
// key. Gateways older than the username field return an empty name.
func (c Admin) Account(ctx context.Context) (role, username string, err error) {
	return c.account(ctx, accountTimeout)
}

func (c Admin) account(ctx context.Context, timeout time.Duration) (role, username string, err error) {
	info, err := c.authInfo(ctx, timeout)
	return info.Role, info.Username, err
}

// AuthInfo is the gateway's description of a credential and its features.
type AuthInfo struct {
	Role         string `json:"role"`
	Username     string `json:"username"`
	GatewayProxy bool   `json:"gatewayProxy"`
	// MachineLabels advertises owner-managed label overlays (D185).
	MachineLabels bool `json:"machineLabels"`
}

// DisplayAuth reads the role and gateway features in one request, with the
// short bound of Role, for display paths such as the dashboard.
func (c Admin) DisplayAuth(ctx context.Context) (AuthInfo, error) {
	return c.authInfo(ctx, roleLookupTimeout)
}

func (c Admin) authInfo(ctx context.Context, timeout time.Duration) (AuthInfo, error) {
	if c.Key == "" {
		return AuthInfo{}, fmt.Errorf("gateway credential is not configured; run control login --gateway URL")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var info AuthInfo
	if err := c.request(ctx, http.MethodGet, "/v1/auth", nil, 0, "application/json", &info); err != nil {
		return AuthInfo{}, fmt.Errorf("authenticate with gateway %s: %w", c.URL, err)
	}
	return info, nil
}

func (c Admin) JSON(ctx context.Context, method, path string, params, result any) error {
	var b []byte
	if params != nil {
		var err error
		b, err = json.Marshal(params)
		if err != nil {
			return err
		}
	}
	return c.request(ctx, method, path, bytes.NewReader(b), int64(len(b)), "application/json", result)
}

func (c Admin) Push(ctx context.Context, dir string) (update.Deployment, error) {
	var manifest update.Manifest
	b, err := os.ReadFile(filepath.Join(dir, "control-manifest.json"))
	if err != nil {
		return update.Deployment{}, err
	}
	if err = json.Unmarshal(b, &manifest); err != nil {
		return update.Deployment{}, err
	}
	if err = manifest.Validate(); err != nil {
		return update.Deployment{}, err
	}
	for _, asset := range manifest.Assets {
		path := filepath.Join(dir, asset.File)
		if err = update.Verify(path, asset); err != nil {
			return update.Deployment{}, err
		}
		f, err := os.Open(path)
		if err != nil {
			return update.Deployment{}, err
		}
		err = c.request(ctx, http.MethodPut, "/v1/admin/updates/blobs/"+asset.SHA256, f, asset.Size, "application/octet-stream", nil)
		_ = f.Close()
		if err != nil {
			return update.Deployment{}, err
		}
	}
	var deployment update.Deployment
	err = c.JSON(ctx, http.MethodPost, "/v1/admin/updates", manifest, &deployment)
	return deployment, err
}
