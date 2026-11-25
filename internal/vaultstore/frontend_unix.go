//go:build darwin || linux

package vaultstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/koltyakov/control/internal/client"
	"golang.org/x/sys/unix"
)

var errOtherFrontend = errors.New("vault belongs to another account")

// loadTrusted reads a marker whose every path component is owned by root or
// one non-root account, which becomes the broker. That account must not be
// the caller, so a file the frontend (or software running as it) could
// create is never accepted, whatever its contents say.
func loadTrusted(source string) (Marker, error) {
	if err := cleanAbs(source); err != nil {
		return Marker{}, err
	}
	d, err := openTrusted(filepath.Dir(source), -1)
	if err != nil {
		return Marker{}, err
	}
	defer func() { _ = d.Close() }()
	owner := d.owner
	if owner < 0 {
		// Root-owned directories: the marker file's owner is the broker.
		var st unix.Stat_t
		if err = unix.Fstatat(d.fd, filepath.Base(source), &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				return Marker{}, fmt.Errorf("%s: %w", source, os.ErrNotExist)
			}
			return Marker{}, err
		}
		owner = int(st.Uid)
	}
	self := hooks.uid()
	if owner == 0 || owner == self {
		return Marker{}, fmt.Errorf("vault marker %s is controlled by this account or root, not a dedicated vault account", source)
	}
	data, err := d.readFile(filepath.Base(source), owner, false)
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
	if m.Broker.UID != strconv.Itoa(owner) {
		return Marker{}, fmt.Errorf("vault marker names broker uid %s but is owned by uid %d", m.Broker.UID, owner)
	}
	if p, err := hooks.lookupID(owner); err != nil || p.Name != m.Broker.Name {
		return Marker{}, errors.New("vault marker broker name does not match its owner account")
	}
	if m.Frontend.UID != strconv.Itoa(self) {
		return Marker{}, fmt.Errorf("%w: vault serves uid %s, not uid %d", errOtherFrontend, m.Frontend.UID, self)
	}
	return m, nil
}

// LoadMarker returns the activated vault marker for a Control home. An error
// matching os.ErrNotExist means a positive determination that no vault
// applies, and legacy plaintext mode is allowed. Any other error, including
// ErrActivationMissing, ErrUnverified and ErrAmbientCredentials, must stop
// the CLI rather than fall back.
//
// Without evidence of opt-in (no local activation entry and nothing at the
// fixed default marker path) it returns immediately, so ordinary homes,
// including symlinked or nested CONTROL_HOME directories, are unaffected.
// Otherwise the frontend copy is never trusted by itself: it is re-verified
// against its trusted source on every load, plaintext credentials and node
// tokens are refused, and the fixed default marker is consulted so a deleted
// or moved home cannot silently downgrade to plaintext mode.
//
// nodeConfigs adds caller-selected node configurations to the ambient check.
func LoadMarker(home string, nodeConfigs ...string) (Marker, error) {
	abs, err := filepath.Abs(home)
	if err != nil {
		return Marker{}, err
	}
	// Only a positive ENOENT for the local entry (any file type, including a
	// dangling link, counts as present) reaches the legacy decision.
	if _, err = os.Lstat(filepath.Join(abs, MarkerName)); errors.Is(err, os.ErrNotExist) {
		return Marker{}, defaultActivation()
	} else if err != nil {
		return Marker{}, unverified(err)
	}
	m, err := loadActivated(abs, nodeConfigs)
	if err != nil {
		// A local entry exists: a later ENOENT is a race or tampering,
		// never permission for plaintext mode.
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
		return Marker{}, errors.New("vault activation was interrupted; rerun vault enable with the same receipt")
	}
	trusted, err := loadTrusted(local.Source)
	if errors.Is(err, os.ErrNotExist) {
		// A rolled-back or removed vault leaves a stale activation that
		// must be cleared deliberately, never ignored.
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

// defaultActivation runs only when the home has no activation. Nothing at
// the fixed default path means no vault; anything there is verified strictly.
//
// os.ErrNotExist is returned only for a positive determination: Lstat of the
// fixed path reports ENOENT, or a verified marker serves another account.
// Anything else at that path, including a dangling symbolic link, or one that
// vanishes during verification, is ErrUnverified.
func defaultActivation() error {
	layout := hooks.defaultLayout()
	if _, err := os.Lstat(layout.MarkerPath()); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no vault: %w", os.ErrNotExist)
	} else if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnverified, layout.MarkerPath(), err)
	}
	_, err := loadTrusted(layout.MarkerPath())
	if err == nil {
		return ErrActivationMissing
	}
	if errors.Is(err, errOtherFrontend) {
		return fmt.Errorf("no vault for this account: %w", os.ErrNotExist)
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
		if errors.Is(err, unix.ENOENT) {
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

// ExportMigration collects the frontend's plaintext credentials into a bundle
// for Import in the broker account. It reads only files this account owns,
// changes nothing, and refuses while a retained-tunnel service is running.
// Pass the bundle over a pipe; never store it.
func ExportMigration(home string) (Bundle, error) {
	self := hooks.uid()
	if self == 0 {
		return Bundle{}, errors.New("run migration as the everyday account, not root")
	}
	principal, err := hooks.lookupID(self)
	if err != nil {
		return Bundle{}, err
	}
	h, err := openHome(home)
	if err != nil {
		return Bundle{}, err
	}
	defer func() { _ = h.Close() }()
	items, err := scanHome(h, true)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Version: bundleVersion, Kind: BundleMigration, Frontend: principal, Items: items}, nil
}

// Activate binds the frontend's Control home to a provisioned vault. A home
// with plaintext credentials needs the broker's signed migration Receipt:
// each plaintext file is deleted only if the receipt covers its exact
// contents. Node tokens and credential environment variables are refused
// before anything changes. A pending activation is written before the first
// deletion, so an interrupted run can only be resumed, never silently fall
// back to plaintext mode; it is finalized after a rescan finds nothing left.
//
// Deletion unlinks files; it does not scrub storage, snapshots or backups.
func Activate(home string, opts ActivateOptions) (Marker, error) {
	source := opts.Source
	if source == "" {
		layout := hooks.defaultLayout()
		source = layout.MarkerPath()
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
		if json.Unmarshal(data, &previous) != nil || previous.Source != source || !previous.equal(m) {
			return Marker{}, errors.New("this home is activated for a different vault; restore or remove that activation first")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Marker{}, err
	}
	items, err := scanHome(h, true)
	if err != nil {
		return Marker{}, err
	}
	local := m
	local.Source = source
	if len(items) > 0 {
		if err = checkReceipt(m, items, opts); err != nil {
			return Marker{}, err
		}
		local.Pending = true
		if err = writeActivation(h, local); err != nil {
			return Marker{}, err
		}
		for _, item := range items {
			if err := removeHomeFile(h, item); err != nil {
				return Marker{}, fmt.Errorf("activation interrupted after partial removal; rerun it with the same receipt: %w", err)
			}
		}
		if err = checkAmbient(h, opts.NodeConfigs); err != nil {
			return Marker{}, fmt.Errorf("activation is still pending: %w", err)
		}
		local.Pending = false
	}
	return local, writeActivation(h, local)
}

func writeActivation(h *dir, m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return h.writeFile(MarkerName, append(data, '\n'), 0o600, false)
}

// checkReceipt requires a verified receipt covering every current plaintext
// file's exact contents, and confirmation of any required revocation.
func checkReceipt(m Marker, items []BundleItem, opts ActivateOptions) error {
	if opts.Receipt == nil {
		var paths []string
		for _, item := range items {
			paths = append(paths, item.Path)
		}
		return fmt.Errorf("%w; migrate them first", &AmbientError{Paths: paths})
	}
	if err := opts.Receipt.Verify(m); err != nil {
		return err
	}
	covered := map[string]bool{}
	for _, r := range opts.Receipt.Items {
		if r.Disposition == Imported || r.Disposition == Superseded || r.Disposition == Discarded {
			covered[r.Kind+"\x00"+r.Path+"\x00"+r.Digest] = true
		}
	}
	var missing []string
	for _, item := range items {
		if !covered[item.Kind+"\x00"+item.Path+"\x00"+item.Digest] {
			missing = append(missing, item.Path)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w; export and import again: %s", ErrIncomplete, strings.Join(missing, ", "))
	}
	if len(opts.Receipt.RevokeRequired) > 0 && !opts.RevocationConfirmed {
		return fmt.Errorf("revoke these credentials at the gateway, then confirm: %s", strings.Join(opts.Receipt.RevokeRequired, "; "))
	}
	return nil
}

// removeHomeFile re-reads the file through no-follow descriptors and deletes
// it only if its contents still match the receipt digest.
func removeHomeFile(h *dir, item BundleItem) error {
	parts := strings.Split(item.Path, "/")
	d := h
	var opened []*dir
	defer func() {
		for _, o := range opened {
			_ = o.Close()
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		next, err := d.homeSub(name)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		d = next
	}
	name := parts[len(parts)-1]
	data, err := d.readHomeFile(name)
	if err != nil {
		return err
	}
	if digest(data) != item.Digest {
		return fmt.Errorf("%w: %s changed", ErrIncomplete, item.Path)
	}
	return d.remove(name)
}

// RestoreLegacy writes a broker Rollback bundle back into the frontend home
// and then removes the activation marker. Existing files are never replaced;
// a file that already holds the same contents is accepted, so an interrupted
// restore can be repeated.
func RestoreLegacy(home string, r io.Reader) error {
	bundle, err := decodeBundle(r, BundleRollback)
	if err != nil {
		return err
	}
	if bundle.Frontend.UID != strconv.Itoa(hooks.uid()) {
		return errors.New("rollback bundle is for a different account")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(abs, 0o700); err != nil {
		return err
	}
	h, err := openHome(abs)
	if err != nil {
		return err
	}
	defer func() { _ = h.Close() }()
	if _, err = scanHome(h, true); err != nil {
		return err
	}
	type restore struct {
		parts []string
		data  []byte
		done  bool
	}
	var plan []restore
	var conflicts []string
	for _, item := range bundle.Items {
		data := item.Data
		parts := strings.Split(item.Path, "/")
		if item.Kind == ItemTunnelService {
			var cfg client.StandaloneConfig
			if err = json.Unmarshal(data, &cfg); err != nil || tunnelScope(cfg) != parts[1] {
				return fmt.Errorf("invalid retained-tunnel login %s", item.Path)
			}
			cfg.StateDir = filepath.Join(abs, "clients")
			cfg.AccountRouting = true
			if data, err = json.Marshal(cfg); err != nil {
				return err
			}
		}
		existing, err := readHomePath(h, parts)
		switch {
		case errors.Is(err, os.ErrNotExist):
			plan = append(plan, restore{parts: parts, data: data})
		case err != nil:
			return err
		case bytes.Equal(existing, data):
			plan = append(plan, restore{parts: parts, data: data, done: true})
		default:
			conflicts = append(conflicts, item.Path)
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w; refusing to overwrite", &AmbientError{Paths: conflicts})
	}
	for _, w := range plan {
		if w.done {
			continue
		}
		if err = writeHomeFile(h, w.parts, w.data); err != nil {
			return fmt.Errorf("restore %s: %w", strings.Join(w.parts, "/"), err)
		}
	}
	if err = h.remove(MarkerName); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func readHomePath(h *dir, parts []string) ([]byte, error) {
	d := h
	var opened []*dir
	defer func() {
		for _, o := range opened {
			_ = o.Close()
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		next, err := d.homeSub(name)
		if errors.Is(err, unix.ENOENT) {
			return nil, os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		opened = append(opened, next)
		d = next
	}
	return d.readHomeFile(parts[len(parts)-1])
}

func writeHomeFile(h *dir, parts []string, data []byte) error {
	d := h
	var opened []*dir
	defer func() {
		for _, o := range opened {
			_ = o.Close()
		}
	}()
	for _, name := range parts[:len(parts)-1] {
		next, err := d.homeMkdir(name)
		if err != nil {
			return err
		}
		opened = append(opened, next)
		d = next
	}
	return d.writeFile(parts[len(parts)-1], data, 0o600, true)
}

// FrontendWarnings describes weaknesses of the boundary that the frontend
// account's own privileges introduce. An administrator, or software riding
// a cached sudo credential, can read the protected profile.
func FrontendWarnings() []string {
	var warnings []string
	if unix.Geteuid() == 0 {
		warnings = append(warnings, "the everyday account is root; the vault provides no separation")
	}
	groups, err := unix.Getgroups()
	if err != nil {
		return append(warnings, "cannot read this account's groups: "+err.Error())
	}
	if found := privilegedGroups(append(groups, unix.Getgid()), groupName); len(found) > 0 {
		warnings = append(warnings, "the everyday account belongs to administrative groups ("+strings.Join(found, ", ")+"); software running with its sudo or admin rights can read the vault profile")
	}
	return warnings
}
