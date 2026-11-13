package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/store"
	"github.com/koltyakov/control/internal/update"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc/mgr"
)

type windowsManagedKey struct{}
type windowsManagedMode struct {
	Mode, Config      string
	Legacy, Bootstrap bool
}

func withManagedNodeProfile(ctx context.Context, config string) context.Context {
	mode, _ := ctx.Value(windowsManagedKey{}).(windowsManagedMode)
	if mode.Config == "" {
		mode.Config, _ = filepath.Abs(config)
	}
	return context.WithValue(ctx, windowsManagedKey{}, mode)
}

func legacyManagedRuntime(ctx context.Context) bool {
	mode, _ := ctx.Value(windowsManagedKey{}).(windowsManagedMode)
	return managedChild || mode.Legacy
}

func migrateLegacyManaged(ctx context.Context, dir string, args []string) (bool, error) {
	mode, _ := ctx.Value(windowsManagedKey{}).(windowsManagedMode)
	profile, err := installation.ReadUpdateStartup(mode.Config, dir)
	if err != nil {
		return false, fmt.Errorf("single-process migration needs installation recovery: %w", err)
	}
	mode.Config, mode.Legacy, mode.Bootstrap = profile.Config, true, !managedChild
	switch profile.Mode {
	case "auto", "system":
		mode.Mode = "service"
	case "user":
		mode.Mode = "user"
	case "process":
		mode.Mode = ""
	}
	var current runtimeBinary
	err = store.Read(filepath.Join(dir, "runtime.json"), &current)
	if errors.Is(err, os.ErrNotExist) || err == nil && current.Version == "" {
		source := current.Path
		if source == "" {
			source, err = os.Executable()
			if err != nil {
				return false, err
			}
		}
		current, err = inspectBinary(ctx, source)
		if err != nil {
			return false, err
		}
		staged := filepath.Join(dir, "updates", current.Asset.SHA256, current.Asset.File)
		if !strings.EqualFold(staged, current.Path) {
			file, err := os.Open(current.Path)
			if err != nil {
				return false, err
			}
			err = update.SaveBinary(file, staged, current.Asset)
			_ = file.Close()
			if err != nil {
				return false, err
			}
			current.Path = staged
		}
		if err = store.Write(filepath.Join(dir, "runtime.json"), current); err != nil {
			return false, err
		}
	} else if err != nil {
		return false, fmt.Errorf("single-process migration needs a verified runtime selection: %w", err)
	}
	if err = update.ValidateExecutable(ctx, current.Path, current.Version, current.Asset); err != nil {
		return false, err
	}
	if current.Version != buildinfo.Current().Version || !update.Matches(buildinfo.Current(), current.Asset) {
		return true, errors.New("legacy startup selection differs from the loaded executable; deploy the selected managed build before refreshing startup")
	}
	if err = store.Write(filepath.Join(dir, "runtime-migration.json"), current); err != nil {
		return false, err
	}
	ctx = context.WithValue(ctx, windowsManagedKey{}, mode)
	handoff, err := prepareManagedHandoff(ctx, dir, args, current, current, func() error { return nil })
	if err != nil {
		_ = os.Remove(filepath.Join(dir, "runtime-migration.json"))
		return false, fmt.Errorf("single-process startup migration blocked: %w", err)
	}
	defer handoff.abort()
	if err = ctx.Err(); err != nil {
		return true, err
	}
	return true, handoff.commit()
}

type windowsHandoffRequest struct {
	ID                string
	Parent            int
	Dir               string
	Args              []string
	Current, Next     runtimeBinary
	Mode, Config      string
	Legacy, Bootstrap bool
}

type windowsManagedHandoff struct {
	input     io.WriteCloser
	committed bool
}

// Retained for legacy launch-path fixtures and callers preparing console images.
func managedExecutable(ctx context.Context, dir string, current runtimeBinary) (string, error) {
	if current.Version == "" {
		var err error
		current, err = inspectBinary(ctx, current.Path)
		if err != nil {
			return "", err
		}
	}
	return update.PrepareWindowsRuntime(dir, current, false)
}

func prepareManagedHandoff(ctx context.Context, dir string, args []string, current, next runtimeBinary, _ func() error) (managedHandoff, error) {
	mode, _ := ctx.Value(windowsManagedKey{}).(windowsManagedMode)
	request := windowsHandoffRequest{ID: identity.NewID(), Parent: os.Getpid(), Dir: dir, Args: args, Current: current, Next: next, Mode: mode.Mode, Config: mode.Config}
	request.Legacy, request.Bootstrap = mode.Legacy, mode.Bootstrap
	if mode.Legacy {
		request.Parent = os.Getppid()
	}
	if mode.Mode == "user" {
		return prepareUserManagedHandoff(ctx, request)
	}
	// Run from the verified versioned source, never the stable path that this
	// helper must replace after its parent exits.
	cmd := exec.Command(next.Path, "__update-handoff")
	// SCM/process startup does not need task-job breakaway. Standalone callers
	// retain their enclosing job's containment; User startup uses its own task.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP, HideWindow: true}
	cmd.Stderr = os.Stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = input.Close()
		}
	}()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	defer func() { _ = output.Close() }()
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	ready := make(chan error, 1)
	go func() {
		if writeErr := json.NewEncoder(input).Encode(request); writeErr != nil {
			ready <- writeErr
			return
		}
		line, readErr := bufio.NewReader(io.LimitReader(output, 2048)).ReadString('\n')
		if readErr == nil && strings.HasPrefix(line, "error: ") {
			readErr = errors.New(strings.TrimSpace(strings.TrimPrefix(line, "error: ")))
		}
		if readErr == nil && line != "ready\n" {
			readErr = errors.New("invalid updater readiness response")
		}
		ready <- readErr
	}()
	prepare, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	timedOut := false
	select {
	case err = <-ready:
	case <-prepare.Done():
		err = prepare.Err()
		timedOut = true
	}
	if err != nil {
		_ = input.Close()
		_ = cmd.Process.Kill() // No commit was sent, so no replacement can have run.
		_ = cmd.Wait()
		_ = output.Close()
		if timedOut {
			<-ready
		}
		return nil, fmt.Errorf("prepare temporary updater: %w", err)
	}
	_ = output.Close()
	if err = cmd.Process.Release(); err != nil {
		return nil, err
	}
	return &windowsManagedHandoff{input: input}, nil
}

func (h *windowsManagedHandoff) abort() {
	if !h.committed {
		_ = h.input.Close()
	}
}

func (h *windowsManagedHandoff) commit() error {
	if err := json.NewEncoder(h.input).Encode("commit"); err != nil {
		return err
	}
	h.committed = true
	return h.input.Close()
}

// runManagedHandoff is deliberately temporary. EOF without an explicit commit,
// failed graceful shutdown, denied restart permissions, or a living parent
// cannot switch the runtime or restart the service.
func runManagedHandoff(ctx context.Context, endpoint ...string) error {
	if len(endpoint) != 0 {
		return runUserManagedHandoff(ctx, endpoint)
	}
	defer func() { _ = os.Stdin.Close() }()
	return runManagedHandoffIO(ctx, os.Stdin, os.Stdout)
}

func runManagedHandoffIO(ctx context.Context, input io.ReadCloser, output io.Writer) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	decoder := json.NewDecoder(io.LimitReader(input, 64<<10))
	var request windowsHandoffRequest
	if err := decodeManagedHandoffInput(ctx, decoder, &request, input); err != nil {
		return err
	}
	if !validManagedReady(managedReady{ID: request.ID, SHA256: request.Next.Asset.SHA256}) || request.Parent <= 0 || request.Parent == os.Getpid() || !filepath.IsAbs(request.Dir) || !filepath.IsAbs(request.Next.Path) || request.Next.Version == "" {
		return errors.New("invalid runtime handoff")
	}
	if request.Mode != "" && request.Mode != "service" && request.Mode != "user" {
		return errors.New("invalid runtime startup mode")
	}
	if request.Mode != "" && !filepath.IsAbs(request.Config) {
		return errors.New("managed restart requires an absolute profile")
	}
	parent, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(request.Parent))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(parent) }()
	if err = update.ValidateExecutable(ctx, request.Next.Path, request.Next.Version, request.Next.Asset); err != nil {
		return err
	}
	var service *mgr.Service
	runtime := installation.WindowsRuntimePath(request.Dir)
	var legacy installation.UpdateStartup
	if request.Legacy {
		legacy, err = installation.ReadUpdateStartup(request.Config, request.Dir)
		if err != nil {
			return err
		}
		expectedMode := legacy.Mode
		if expectedMode == "auto" || expectedMode == "system" {
			expectedMode = "service"
		}
		if expectedMode == "process" {
			expectedMode = ""
		}
		if expectedMode != request.Mode {
			return errors.New("legacy startup mode changed before migration")
		}
		expectedParent := legacy.Binary
		if request.Bootstrap {
			if request.Mode != "user" {
				return errors.New("only User startup has a legacy bootstrap")
			}
			expectedParent, err = installation.LegacyUpdateUserBootstrap(ctx, request.Config, legacy.Binary, request.Dir)
			if err != nil {
				return err
			}
		}
		imageParent, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(request.Parent))
		if err != nil {
			return err
		}
		name := make([]uint16, 32768)
		size := uint32(len(name))
		err = windows.QueryFullProcessImageName(imageParent, 0, &name[0], &size)
		_ = windows.CloseHandle(imageParent)
		if err != nil {
			return err
		}
		if !strings.EqualFold(filepath.Clean(windows.UTF16ToString(name[:size])), filepath.Clean(expectedParent)) {
			return errors.New("legacy parent no longer belongs to the installed startup")
		}
	}
	switch request.Mode {
	case "service":
		if request.Legacy {
			service, err = installation.OpenLegacyUpdateService(request.Config, legacy.Binary)
		} else {
			service, err = installation.OpenUpdateService(request.Config, runtime)
		}
		if err != nil {
			return err
		}
		defer func() { _ = service.Close() }()
	case "user":
		if request.Legacy {
			err = installation.CheckLegacyUpdateUserTask(ctx, request.Config, legacy.Binary, request.Dir)
		} else {
			err = installation.CheckUpdateUserTask(ctx, request.Config, runtime)
		}
		if err != nil {
			return err
		}
	}
	if _, err = fmt.Fprintln(output, "ready"); err != nil {
		return err
	}
	if output == os.Stdout {
		_ = os.Stdout.Close()
		os.Stdout = os.Stderr
	}
	// Reading this pipe cannot outlive the preparation deadline, even if a
	// malformed caller never commits or exits.
	var command string
	if err = decodeManagedHandoffInput(ctx, decoder, &command, input); err != nil {
		return err
	}
	if command != "commit" {
		return errors.New("runtime handoff was not committed")
	}
	for {
		result, err := windows.WaitForSingleObject(parent, 100)
		if err != nil {
			return err
		}
		if result == windows.WAIT_OBJECT_0 {
			break
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
	if request.Legacy {
		installationLock := flock.New(request.Config + ".install.lock")
		defer func() { _ = installationLock.Close() }()
		locked, err := installationLock.TryLockContext(ctx, 100*time.Millisecond)
		if err != nil {
			return err
		}
		if !locked {
			return errors.New("installation changed during legacy shutdown")
		}
		fresh, err := installation.ReadUpdateStartup(request.Config, request.Dir)
		if err != nil {
			return err
		}
		if fresh != legacy {
			return errors.New("legacy installation identity changed during shutdown")
		}
	}
	lock := flock.New(filepath.Join(request.Dir, "runtime.lock"))
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 100*time.Millisecond)
	if err != nil {
		return err
	}
	if !locked {
		return errors.New("runtime lock unavailable after shutdown")
	}
	var saved runtimeBinary
	requestFile := "runtime-request.json"
	if request.Legacy {
		requestFile = "runtime-migration.json"
	}
	if err = store.Read(filepath.Join(request.Dir, requestFile), &saved); err != nil {
		return err
	}
	if saved != request.Next {
		return errors.New("runtime request changed during shutdown")
	}
	var selected runtimeBinary
	err = store.Read(filepath.Join(request.Dir, "runtime.json"), &selected)
	if err != nil && (!errors.Is(err, os.ErrNotExist) || request.Current.Version != "") {
		return err
	}
	if err == nil && selected != request.Current {
		return errors.New("runtime selection changed during shutdown")
	}
	if err = update.ValidateExecutable(ctx, request.Next.Path, request.Next.Version, request.Next.Asset); err != nil {
		return err
	}
	if request.Current != request.Next {
		if err = update.SaveRuntimeSwitch(request.Dir, request.Current, request.Next); err != nil {
			return err
		}
	}
	if _, err = update.PrepareWindowsRuntime(request.Dir, request.Next, request.Mode == "user"); err != nil {
		return err
	}
	marker := managedReady{ID: request.ID, SHA256: request.Next.Asset.SHA256}
	if err = store.Write(filepath.Join(request.Dir, "runtime-starting.json"), marker); err != nil {
		return err
	}
	if err = os.Remove(filepath.Join(request.Dir, requestFile)); err != nil {
		return err
	}
	if err = lock.Close(); err != nil {
		return err
	}
	switch request.Mode {
	case "service":
		if request.Legacy {
			if err = installation.SwitchLegacyUpdateService(service, request.Config, legacy.Binary, runtime); err != nil {
				return err
			}
		}
		err = installation.RestartUpdateService(ctx, service)
	case "user":
		if request.Legacy {
			if err = installation.SwitchLegacyUpdateUserTask(ctx, request.Config, legacy.Binary, request.Dir, runtime); err != nil {
				return err
			}
		}
		err = installation.RestartUpdateUserTask(ctx, request.Config, runtime)
	default:
		cmd := exec.Command(runtime, request.Args...)
		processutil.HideWindow(cmd)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err = cmd.Start(); err == nil {
			err = cmd.Process.Release()
		}
	}
	if err != nil {
		return err
	} // Never retry an uncertain service/process start.
	return waitManagedReady(ctx, request.Dir, marker)
}

func decodeManagedHandoffInput(ctx context.Context, decoder *json.Decoder, target any, input io.Closer) error {
	done := make(chan error, 1)
	go func() { done <- decoder.Decode(target) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = input.Close()
		<-done
		return ctx.Err()
	}
}

func waitManagedReady(ctx context.Context, dir string, want managedReady) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ready managedReady
		err := store.Read(managedReadyPath(dir, want.ID), &ready)
		if err == nil && ready == want {
			return os.Remove(managedReadyPath(dir, want.ID))
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("updated runtime did not report readiness; retained selection requires recovery: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
