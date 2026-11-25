//go:build windows

package vaultstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var errOtherFrontend = errors.New("vault belongs to another account")

// loadTrusted reads the marker in the fixed vault root. The root must pass
// checkRoot, so its broker is the single dedicated account an administrator
// assigned to it; the marker file must be owned by that account and name it.
// The broker must not be the caller, so a file the frontend (or software
// running as it) could create is never accepted, whatever its contents say.
func loadTrusted(source string) (Marker, error) {
	if err := checkFrontendProcess(); err != nil {
		return Marker{}, err
	}
	if _, _, err := splitAbs(source); err != nil {
		return Marker{}, err
	}
	if !strings.EqualFold(filepath.Base(source), MarkerName) {
		return Marker{}, fmt.Errorf("vault marker must be named %s", MarkerName)
	}
	layout, err := requireCanonical(filepath.Dir(source))
	if err != nil {
		return Marker{}, err
	}
	root, broker, err := openRootDir(layout.Root, "", dirRead)
	if err != nil {
		return Marker{}, err
	}
	defer func() { _ = root.Close() }()
	self, err := hooks.sid()
	if err != nil {
		return Marker{}, err
	}
	if broker == self {
		return Marker{}, fmt.Errorf("vault root %s is assigned to this account, not a dedicated vault account", layout.Root)
	}
	data, err := root.readFile(MarkerName, broker, false)
	if err != nil {
		return Marker{}, err
	}
	var m Marker
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		return Marker{}, errors.New("invalid vault marker")
	}
	if err = m.Validate(); err != nil {
		return Marker{}, err
	}
	if m.Source != "" || m.Pending {
		return Marker{}, errors.New("published vault marker must not carry activation fields")
	}
	if m.Broker.UID != broker {
		return Marker{}, fmt.Errorf("vault marker names broker %s but the vault root belongs to %s", m.Broker.UID, broker)
	}
	if p, err := hooks.lookupSID(broker); err != nil || p.Name != m.Broker.Name {
		return Marker{}, errors.New("vault marker broker name does not match its owner account")
	}
	if m.Frontend.UID != self {
		return Marker{}, fmt.Errorf("%w: vault serves %s, not %s", errOtherFrontend, m.Frontend.UID, self)
	}
	return m, nil
}

// LoadMarker returns the activated vault marker for a Control home. An error
// matching os.ErrNotExist means a positive determination that no vault
// applies, and legacy plaintext mode is allowed. Any other error, including
// ErrActivationMissing, ErrUnverified and ErrAmbientCredentials, must stop
// the CLI rather than fall back.
//
// The frontend copy is never trusted by itself: it is re-verified against the
// fixed trusted marker on every load, plaintext credentials and node tokens
// are refused, and the fixed marker is consulted when the home has no
// activation so a deleted or moved home cannot silently downgrade.
//
// nodeConfigs adds caller-selected node configurations to the ambient check.
func LoadMarker(home string, nodeConfigs ...string) (Marker, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return Marker{}, err
	}
	if _, err = os.Lstat(filepath.Join(abs, MarkerName)); errors.Is(err, os.ErrNotExist) {
		return Marker{}, defaultActivation()
	} else if err != nil {
		return Marker{}, unverified(err)
	}
	m, err := loadActivated(abs, nodeConfigs)
	if err != nil {
		return Marker{}, unverified(err)
	}
	return m, nil
}

// unverified rewraps an error so it never matches os.ErrNotExist.
func unverified(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %v", ErrUnverified, err)
	}
	return err
}

func loadActivated(abs string, nodeConfigs []string) (Marker, error) {
	h, err := openHome(abs)
	if err != nil {
		return Marker{}, err
	}
	defer func() { _ = h.Close() }()
	data, err := h.readHomeFile(MarkerName)
	if err != nil {
		return Marker{}, err
	}
	var local Marker
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&local); err != nil || local.Source == "" {
		return Marker{}, errors.New("invalid vault activation marker")
	}
	if local.Pending {
		return Marker{}, errors.New("vault activation was interrupted; rerun vault enable")
	}
	trusted, err := loadTrusted(local.Source)
	if errors.Is(err, os.ErrNotExist) {
		return Marker{}, fmt.Errorf("vault marker %s is gone; restore the legacy profile or activate again", local.Source)
	}
	if err != nil {
		return Marker{}, err
	}
	if !trusted.equal(local) {
		return Marker{}, errors.New("activation marker differs from the trusted vault marker; activate again")
	}
	if err = checkAmbient(h, nodeConfigs); err != nil {
		return Marker{}, err
	}
	trusted.Source = local.Source
	return trusted, nil
}

// defaultActivation runs only when the home has no activation. The fixed
// location is always walked through handles, never probed by a path that a
// per-user drive mapping could redirect.
//
// os.ErrNotExist is returned only for a positive determination: the fixed
// location cannot exist on this host; the root or its marker is absent; the
// root is not administrator-owned or not readable by this account (any
// account can create a directory in ProgramData, the broker refuses such a
// root, and a provisioned root always lets its frontend read it); or a
// verified marker serves another account. Anything else is ErrUnverified.
func defaultActivation() error {
	layout, err := hooks.defaultLayout()
	if err != nil {
		return fmt.Errorf("no vault location on this host (%v): %w", err, os.ErrNotExist)
	}
	_, err = loadTrusted(layout.MarkerPath())
	switch {
	case err == nil:
		return ErrActivationMissing
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("no vault: %w", os.ErrNotExist)
	case errors.Is(err, errOtherFrontend), errors.Is(err, errNotAdministered), errors.Is(err, errRootUnreadable):
		return fmt.Errorf("no vault for this account (%v): %w", err, os.ErrNotExist)
	}
	return fmt.Errorf("%w: %s: %v", ErrUnverified, layout.MarkerPath(), err)
}

// CheckAmbient reports plaintext credentials visible to the frontend: login
// files, task owner keys, retained-tunnel logins, definitions and management
// tokens in home; accessible node configurations holding a token; and
// credential environment variables including CONTROL_TOKEN.
func CheckAmbient(home string, nodeConfigs ...string) error {
	abs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	h, err := openHome(abs)
	if errors.Is(err, os.ErrNotExist) {
		return checkAmbientSources(abs, nil, nodeConfigs)
	}
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	return checkAmbient(h, nodeConfigs)
}

func checkAmbient(h *dir, nodeConfigs []string) error {
	items, err := scanHome(h, false)
	if err != nil {
		return err
	}
	var paths []string
	for _, item := range items {
		paths = append(paths, item.Path)
	}
	return checkAmbientSources(h.path, paths, nodeConfigs)
}

// scanHome lists the frontend's plaintext credential files. With
// requireIdle it refuses while a retained-tunnel service holds its lock.
func scanHome(h *dir, requireIdle bool) ([]BundleItem, error) {
	var items []BundleItem
	var busy []error
	add := func(d *dir, kind, rel, name string) error {
		data, err := d.readHomeFile(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if kind == ItemAccount || kind == ItemUpdate {
			var a adminFile
			if json.Unmarshal(data, &a) == nil && a.Key == "" {
				return nil
			}
		}
		items = append(items, BundleItem{Kind: kind, Path: rel, Digest: digest(data), Data: data})
		return nil
	}
	if err := add(h, ItemAccount, "admin.json", "admin.json"); err != nil {
		return nil, err
	}
	if err := add(h, ItemUpdate, "update-admin.json", "update-admin.json"); err != nil {
		return nil, err
	}
	for _, area := range []struct {
		dir   string
		files [][2]string
	}{
		{"clients", [][2]string{{ItemOwner, "identity.key"}}},
		{"tunnels", [][2]string{{ItemTunnelService, "service.json"}, {ItemTunnels, "tunnels.json"}, {ItemTunnelEndpoint, "endpoint.json"}}},
	} {
		d, err := h.homeSub(area.dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries, err := d.list()
		for _, e := range entries {
			if err != nil {
				break
			}
			if !validScope(e.Name()) {
				continue
			}
			var scope *dir
			if scope, err = d.homeSub(e.Name()); err != nil {
				break
			}
			for _, f := range area.files {
				if err = add(scope, f[0], area.dir+"/"+e.Name()+"/"+f[1], f[1]); err != nil {
					break
				}
			}
			if err == nil && requireIdle && area.dir == "tunnels" {
				if lockErr := scope.probeLock("service.lock"); lockErr != nil {
					busy = append(busy, lockErr)
				}
			}
			_ = scope.Close()
		}
		_ = d.Close()
		if err != nil {
			return nil, err
		}
	}
	if len(busy) > 0 {
		return nil, errors.Join(busy...)
	}
	return items, nil
}

// ExportMigration is refused on Windows before anything is read; see
// errWindowsMigration.
func ExportMigration(string) (Bundle, error) { return Bundle{}, errWindowsMigration }

// Activate binds the frontend's Control home to the provisioned vault. On
// Windows only a clean home can be activated: any plaintext login, owner key,
// retained-tunnel state, accessible node token or credential environment
// variable refuses activation before anything changes, and a migration
// receipt is refused. Nothing is deleted.
func Activate(home string, opts ActivateOptions) (Marker, error) {
	source := opts.Source
	if source == "" {
		layout, err := hooks.defaultLayout()
		if err != nil {
			return Marker{}, err
		}
		source = layout.MarkerPath()
	}
	if opts.Receipt != nil {
		return Marker{}, errWindowsMigration
	}
	m, err := loadTrusted(source)
	if err != nil {
		return Marker{}, err
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return Marker{}, err
	}
	if err = checkAmbientSources(abs, nil, opts.NodeConfigs); err != nil {
		return Marker{}, err
	}
	if err = os.MkdirAll(abs, 0o700); err != nil {
		return Marker{}, err
	}
	h, err := openHome(abs)
	if err != nil {
		return Marker{}, err
	}
	defer func() { _ = h.Close() }()
	if data, err := h.readHomeFile(MarkerName); err == nil {
		var previous Marker
		if json.Unmarshal(data, &previous) != nil || !strings.EqualFold(previous.Source, source) || !previous.equal(m) {
			return Marker{}, errors.New("this home is activated for a different vault; restore or remove that activation first")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Marker{}, err
	}
	items, err := scanHome(h, true)
	if err != nil {
		return Marker{}, err
	}
	if len(items) > 0 {
		paths := make([]string, 0, len(items))
		for _, item := range items {
			paths = append(paths, item.Path)
		}
		return Marker{}, fmt.Errorf("%w; %w", &AmbientError{Paths: paths}, errWindowsMigration)
	}
	local := m
	local.Source = source
	data, err := json.MarshalIndent(local, "", "  ")
	if err != nil {
		return Marker{}, err
	}
	sd, err := homeSD()
	if err != nil {
		return Marker{}, err
	}
	if err = h.writeFile(MarkerName, append(data, '\n'), sd, false); err != nil {
		return Marker{}, err
	}
	return local, nil
}

// RestoreLegacy accepts a Windows rollback bundle, which carries no
// credentials, and removes the activation marker. Afterwards log in again in
// this account with a fresh key.
func RestoreLegacy(home string, r io.Reader) error {
	bundle, err := decodeRollback(r)
	if err != nil {
		return err
	}
	self, err := hooks.sid()
	if err != nil {
		return err
	}
	if bundle.Frontend.UID != self {
		return errors.New("rollback bundle is for a different account")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	h, err := openHome(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	if err = h.remove(MarkerName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// FrontendWarnings describes weaknesses of the boundary that the frontend
// account's own privileges introduce. An administrator, including a
// UAC-filtered one that can elevate, can read the protected profile.
func FrontendWarnings() []string {
	f, err := currentToken()
	if err != nil {
		return []string{"cannot inspect this account's token: " + err.Error()}
	}
	var warnings []string
	if f.elevated || f.integrity >= integrityHigh {
		warnings = append(warnings, "this process is elevated; elevated software can read the vault profile")
	}
	if found := privilegedGroups(f.groups, localPrivilegedGroups()); len(found) > 0 {
		warnings = append(warnings, "the everyday account belongs to administrative groups ("+strings.Join(found, ", ")+"); software that elevates or uses those rights can read the vault profile")
	}
	return warnings
}
