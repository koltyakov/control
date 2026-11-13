package installation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/koltyakov/control/internal/update"
	"golang.org/x/sys/unix"
)

// Native cp preserves macOS ACLs as well as extended attributes. Rewrite only
// the private staged inode, never the installed or running executable. chmod
// and xattrs alone would silently lose ACLs that Darwin stores separately.
func prepareLegacyUnixImage(ctx context.Context, original string, source *os.File, staged string, asset update.Asset) error {
	if err := exec.CommandContext(ctx, "/bin/cp", "-p", original, staged).Run(); err != nil {
		return err
	}
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(source, asset.Size+1))
	if err == nil && (n != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256) {
		err = errors.New("runtime source changed during startup preparation")
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func legacyUnixBinding(ctx context.Context, profile UpdateStartup) (func() error, error) {
	var path, target, expected string
	switch profile.Mode {
	case "user", "auto":
		var err error
		path, err = servicePath()
		if err != nil {
			return nil, err
		}
		target = fmt.Sprintf("gui/%d/com.koltyakov.control", os.Getuid())
		expected = userLaunchPlist(profile.Binary, profile.Config)
	case "system":
		if os.Geteuid() != 0 {
			return nil, errors.New("system launchd migration requires its existing root execution context")
		}
		label := "com.koltyakov.control." + systemServiceID(profile.Config)
		path, target = filepath.Join("/Library/LaunchDaemons", label+".plist"), "system/"+label
		expected = systemLaunchPlist(profile.Binary, profile.Config)
	default:
		return nil, errors.New("startup is not a supported launchd profile")
	}
	parent := os.Getppid()
	if parent <= 1 {
		return nil, errors.New("legacy launchd supervisor is missing")
	}
	before, err := unix.SysctlKinfoProc("kern.proc.pid", parent)
	if err != nil {
		return nil, err
	}
	check := func() error {
		current, err := unix.SysctlKinfoProc("kern.proc.pid", parent)
		if err != nil || current.Proc.P_starttime != before.Proc.P_starttime || current.Eproc.Ppid != 1 || current.Eproc.Ucred.Uid != uint32(os.Geteuid()) {
			return errors.New("legacy supervisor identity or launchd parent changed")
		}
		plist, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(plist, []byte(expected)) {
			return errors.New("launchd definition is not the exact owned restart-on-failure profile")
		}
		output, err := exec.CommandContext(ctx, "/bin/launchctl", "print", target).Output()
		if err != nil || !matchesLegacyLaunchd(output, path, profile.Binary, profile.Config, parent) {
			return errors.New("loaded launchd job does not match the installed supervisor")
		}
		image, err := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(parent), "-o", "comm=").Output()
		if err != nil || strings.TrimSpace(string(image)) != profile.Binary {
			return errors.New("legacy parent image is not the installed startup executable")
		}
		return ctx.Err()
	}
	return check, check()
}

func matchesLegacyLaunchd(output []byte, path, binary, config string, parent int) bool {
	text := string(output)
	for _, line := range []string{
		"\tpath = " + path, "\tprogram = " + binary, "\tpid = " + strconv.Itoa(parent),
		"\targuments = {\n\t\t" + binary + "\n\t\tnode\n\t\t--config\n\t\t" + config + "\n\t}",
	} {
		if !strings.Contains(text, "\n"+line+"\n") {
			return false
		}
	}
	return true
}
