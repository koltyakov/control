package installation

import (
	"context"
	"os"
	"path/filepath"
)

func retiredSystemdService(p installedNode) (string, string, []string, error) {
	if p.Mode == "system" {
		name := "control-node-" + systemServiceID(p.Config) + ".service"
		return filepath.Join("/etc/systemd/system", name), name, nil, nil
	}
	path, err := servicePath()
	return path, "control-node.service", []string{"--user"}, err
}

func launchRetiredUninstall(ctx context.Context, helper string, args []string, p installedNode) error {
	if p.Mode == "process" {
		return launchDetachedUninstall(helper, args)
	}
	path, _, flags, err := retiredSystemdService(p)
	if err != nil {
		return err
	}
	owned, err := checkRetiredServiceFile(path, "--config "+systemdQuote(p.Config))
	if err != nil {
		return err
	}
	if !owned {
		return launchDetachedUninstall(helper, args)
	}
	// A detached child remains in the node's cgroup and would be killed when
	// systemd stops the supervisor. A transient service has its own cgroup.
	flags = append(flags, "--collect", "--unit=control-"+p.ID+"-"+filepath.Base(filepath.Dir(helper)), "--property=Type=exec", "--", helper)
	return run(ctx, "systemd-run", append(flags, args...)...)
}

func removeRetiredStartup(ctx context.Context, p installedNode) error {
	if p.Mode == "process" {
		return nil
	}
	path, name, flags, err := retiredSystemdService(p)
	if err != nil {
		return err
	}
	owned, err := checkRetiredServiceFile(path, "--config "+systemdQuote(p.Config))
	if err != nil || !owned {
		return err
	}
	if err = run(ctx, "systemctl", append(flags, "disable", name)...); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil {
		return err
	}
	return run(ctx, "systemctl", append(flags, "daemon-reload")...)
}

func finishRetiredUninstall(_ context.Context, _ installedNode) error { return nil }

func finishRetiredHelper(_ context.Context, _, _ string) {}
