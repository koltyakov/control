package installation

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func retiredLaunchService(p installedNode) (string, string, error) {
	if p.Mode == "system" {
		label := "com.koltyakov.control." + systemServiceID(p.Config)
		return filepath.Join("/Library/LaunchDaemons", label+".plist"), "system/" + label, nil
	}
	path, err := servicePath()
	return path, fmt.Sprintf("gui/%d/com.koltyakov.control", os.Getuid()), err
}

func launchRetiredUninstall(ctx context.Context, helper string, args []string, p installedNode) error {
	if p.Mode == "process" {
		return launchDetachedUninstall(helper, args)
	}
	path, _, err := retiredLaunchService(p)
	if err != nil {
		return err
	}
	owned, err := checkRetiredServiceFile(path, "<string>"+xmlText(p.Config)+"</string>")
	if err != nil {
		return err
	}
	if !owned {
		return launchDetachedUninstall(helper, args)
	}
	// Submit a separate one-shot launchd job so bootout of the node cannot kill
	// cleanup along with the node's descendants.
	label := retiredHelperLabel(helper, p.ID)
	return run(ctx, "launchctl", append([]string{"submit", "-l", label, "--", helper}, args...)...)
}

func retiredHelperLabel(helper, id string) string {
	return "com.koltyakov.control." + id + "." + filepath.Base(filepath.Dir(helper))
}

func finishRetiredHelper(ctx context.Context, helper, id string) {
	label := retiredHelperLabel(helper, id)
	if exec.CommandContext(ctx, "launchctl", "list", label).Run() == nil {
		// This may terminate the helper itself. All node locks and log writes
		// have already completed, so remove this job only as the final action.
		_ = run(ctx, "launchctl", "remove", label)
	}
}

func removeRetiredStartup(_ context.Context, p installedNode) error {
	if p.Mode == "process" {
		return nil
	}
	path, _, err := retiredLaunchService(p)
	if err != nil {
		return err
	}
	owned, err := checkRetiredServiceFile(path, "<string>"+xmlText(p.Config)+"</string>")
	if err != nil || !owned {
		return err
	}
	return os.Remove(path)
}

func finishRetiredUninstall(ctx context.Context, p installedNode) error {
	if p.Mode == "process" {
		return nil
	}
	_, target, err := retiredLaunchService(p)
	if err != nil {
		return err
	}
	if exec.CommandContext(ctx, "launchctl", "print", target).Run() != nil {
		return nil
	}
	return run(ctx, "launchctl", "bootout", target)
}
