//go:build !windows

package installation

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/koltyakov/control/internal/node"
	"github.com/koltyakov/control/internal/store"
)

func CheckServiceMode(mode string) error {
	if err := ValidateServiceMode(mode); err != nil {
		return err
	}
	if mode == "system" && os.Geteuid() != 0 {
		return errors.New("system startup requires root; rerun with sudo")
	}
	return nil
}

type unixStartupProfile struct {
	Mode string `json:"mode"`
}

func resolveServiceMode(config, mode string) (string, error) {
	if mode != "" {
		return mode, nil
	}
	var profile unixStartupProfile
	if err := store.Read(config+".startup.json", &profile); err != nil {
		if os.IsNotExist(err) {
			return "auto", nil
		}
		return "", err
	}
	if err := ValidateServiceMode(profile.Mode); err != nil || profile.Mode == "" {
		return "", errors.New("invalid saved startup mode")
	}
	return profile.Mode, nil
}

func finishServiceStop(_ context.Context, _, _ string) error { return nil }

func checkServiceProfile(config, mode string) error {
	var profile unixStartupProfile
	if err := store.Read(config+".startup.json", &profile); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if (profile.Mode == "system") != (mode == "system") {
		return errors.New("changing between user and system startup requires a separate profile; stop and uninstall the existing startup before installing another profile")
	}
	return nil
}

func platformService(ctx context.Context, operation, binary, config, mode string, cfg node.Config) (bool, error) {
	if operation == "start" {
		if err := checkServiceProfile(config, mode); err != nil {
			return true, err
		}
	}
	if mode != "system" || operation == "status" {
		return false, nil
	}
	if operation != "start" && operation != "stop" && operation != "uninstall" {
		return false, nil
	}
	if err := CheckServiceMode(mode); err != nil {
		return true, err
	}
	if operation == "start" {
		if err := store.Write(config+".startup.json", unixStartupProfile{Mode: mode}); err != nil {
			return true, err
		}
	}
	return true, systemService(ctx, operation, binary, config, cfg)
}

func systemServiceID(config string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(config)))
	return fmt.Sprintf("%x", sum[:8])
}
