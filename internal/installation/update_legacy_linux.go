package installation

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/koltyakov/control/internal/update"
)

func prepareLegacyUnixImage(_ context.Context, _ string, source *os.File, staged string, asset update.Asset) error {
	return update.SaveBinary(source, staged, asset)
}

func legacyUnixBinding(ctx context.Context, profile UpdateStartup) (func() error, error) {
	var path, name, expected string
	var flags []string
	switch profile.Mode {
	case "user", "auto":
		var err error
		path, err = servicePath()
		if err != nil {
			return nil, err
		}
		name, flags = "control-node.service", []string{"--user"}
		expected = userSystemdUnit(profile.Binary, profile.Config)
	case "system":
		if os.Geteuid() != 0 {
			return nil, errors.New("systemd System migration requires its existing root execution context")
		}
		name = "control-node-" + systemServiceID(profile.Config) + ".service"
		path = filepath.Join("/etc/systemd/system", name)
		expected = systemdSystemUnit(profile.Binary, profile.Config)
	default:
		return nil, errors.New("startup is not a supported systemd profile")
	}
	parent := os.Getppid()
	if parent <= 1 {
		return nil, errors.New("container PID 1 and unmanaged legacy startup require an explicit restart")
	}
	proc := filepath.Join("/proc", strconv.Itoa(parent))
	before, err := linuxProcessStart(proc)
	if err != nil {
		return nil, err
	}
	check := func() error {
		current, err := linuxProcessStart(proc)
		if err != nil || current != before {
			return errors.New("legacy supervisor process identity changed")
		}
		image, err := os.Readlink(filepath.Join(proc, "exe"))
		if err != nil || image != profile.Binary {
			return errors.New("legacy parent image is not the installed startup executable")
		}
		arguments, err := os.ReadFile(filepath.Join(proc, "cmdline"))
		if err != nil || !bytes.Equal(arguments, []byte(strings.Join([]string{profile.Binary, "node", "--config", profile.Config, ""}, "\x00"))) {
			return errors.New("legacy supervisor arguments differ from installed startup")
		}
		unit, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(unit, []byte(expected)) {
			return errors.New("systemd definition is not the exact owned restart-on-failure profile")
		}
		status, err := os.ReadFile(filepath.Join(proc, "status"))
		if err != nil {
			return err
		}
		uidOK := false
		for _, line := range strings.Split(string(status), "\n") {
			if !strings.HasPrefix(line, "Uid:") {
				continue
			}
			fields := strings.Fields(line)
			uidOK = len(fields) == 5
			for _, uid := range fields[1:] {
				uidOK = uidOK && uid == strconv.Itoa(os.Geteuid())
			}
		}
		if !uidOK {
			return errors.New("legacy supervisor account differs from the executing profile")
		}
		args := append(append([]string(nil), flags...), "show", name, "--property=MainPID,ActiveState,FragmentPath,DropInPaths,NeedDaemonReload,Restart,ControlGroup")
		output, err := exec.CommandContext(ctx, "/usr/bin/systemctl", args...).Output()
		if err != nil {
			return errors.New("cannot bind the running supervisor to systemd")
		}
		properties := make(map[string]string)
		for _, line := range strings.Split(string(output), "\n") {
			if key, value, ok := strings.Cut(line, "="); ok {
				properties[key] = value
			}
		}
		cgroup, err := os.ReadFile(filepath.Join(proc, "cgroup"))
		if err != nil {
			return err
		}
		if !matchesLegacySystemd(properties, path, parent, string(cgroup)) {
			return errors.New("loaded systemd job does not match the installed supervisor")
		}
		return ctx.Err()
	}
	return check, check()
}

func linuxProcessStart(proc string) (string, error) {
	data, err := os.ReadFile(filepath.Join(proc, "stat"))
	if err != nil {
		return "", err
	}
	end := strings.LastIndex(string(data), ") ")
	if end < 0 {
		return "", errors.New("invalid supervisor process metadata")
	}
	fields := strings.Fields(string(data[end+2:]))
	if len(fields) <= 19 {
		return "", errors.New("truncated supervisor process metadata")
	}
	return fields[19], nil
}

func matchesLegacySystemd(properties map[string]string, path string, parent int, cgroup string) bool {
	if properties["MainPID"] != strconv.Itoa(parent) || properties["ActiveState"] != "active" ||
		properties["FragmentPath"] != path || properties["DropInPaths"] != "" ||
		properties["NeedDaemonReload"] != "no" || properties["Restart"] != "on-failure" || properties["ControlGroup"] == "" {
		return false
	}
	for _, line := range strings.Split(cgroup, "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && (fields[1] == "" || fields[1] == "name=systemd") && fields[2] == properties["ControlGroup"] {
			return true
		}
	}
	return false
}
