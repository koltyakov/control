// control-bundle builds cross-platform bundles consumed by the update API.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/update"
)

func main() {
	if err := build(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func build() error {
	now := time.Now().UTC()
	dir := flag.String("out", "dist", "bundle directory")
	binary := flag.String("binary", "", "build a single native executable instead of a bundle")
	repository := flag.String("repository", "", "embedded GitHub owner/repository; defaults to the source repository")
	version := flag.String("version", "dev-"+now.Format("20060102T150405.000000000Z"), "release tag or development version")
	platforms := flag.String("platforms", "linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64", "comma-separated OS/architecture pairs")
	flag.Parse()
	if *repository == "" {
		*repository = sourceRepository()
	}
	if !repositoryPattern.MatchString(*repository) {
		return fmt.Errorf("invalid GitHub repository %q", *repository)
	}
	ldflags := "-s -w -X github.com/koltyakov/control/internal/buildinfo.Version=" + *version +
		" -X github.com/koltyakov/control/internal/buildinfo.ReleaseRepo=" + *repository +
		" -X github.com/koltyakov/control/internal/buildinfo.BuildTime=" + now.Format(time.RFC3339Nano)
	manifest := update.Manifest{Version: *version, CreatedAt: now}
	if *binary != "" {
		*platforms = envOr("GOOS", runtime.GOOS) + "/" + envOr("GOARCH", runtime.GOARCH)
	}
	for _, platform := range strings.Split(*platforms, ",") {
		parts := strings.Split(platform, "/")
		if len(parts) != 2 {
			return fmt.Errorf("invalid platform %q", platform)
		}
		manifest.Assets = append(manifest.Assets, update.Asset{OS: parts[0], Arch: parts[1], File: update.AssetName(parts[0], parts[1]), Size: 1, SHA256: strings.Repeat("0", 64)})
	}
	if err := manifest.Validate(); err != nil {
		return err
	}
	if *binary != "" {
		a := manifest.Assets[0]
		return buildBinary(*binary, a.OS, a.Arch, ldflags)
	}
	if err := os.MkdirAll(*dir, 0700); err != nil {
		return err
	}
	var checksums strings.Builder
	for i := range manifest.Assets {
		a := &manifest.Assets[i]
		path := filepath.Join(*dir, a.File)
		fmt.Fprintln(os.Stderr, "building", a.OS+"/"+a.Arch)
		if err := buildBinary(path, a.OS, a.Arch, ldflags); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		a.Size, err = io.Copy(hash, f)
		_ = f.Close()
		if err != nil {
			return err
		}
		a.SHA256 = hex.EncodeToString(hash.Sum(nil))
		fmt.Fprintf(&checksums, "%s  %s\n", a.SHA256, a.File)
	}
	if err := os.WriteFile(filepath.Join(*dir, "checksums.txt"), []byte(checksums.String()), 0600); err != nil {
		return err
	}
	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(*dir, "control-manifest.json"), append(b, '\n'), 0600)
}

func buildBinary(path, osName, arch, ldflags string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", path, "./cmd/control")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+osName, "GOARCH="+arch)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func sourceRepository() string {
	if output, err := exec.Command("git", "remote", "get-url", "origin").Output(); err == nil {
		if repo := githubRepository(strings.TrimSpace(string(output))); repo != "" {
			return repo
		}
	}
	if repo := os.Getenv("GITHUB_REPOSITORY"); repositoryPattern.MatchString(repo) {
		return repo
	}
	return buildinfo.ReleaseRepo
}

func githubRepository(remote string) string {
	var repo string
	if path, ok := strings.CutPrefix(remote, "git@github.com:"); ok {
		repo = path
	} else {
		u, err := url.Parse(remote)
		if err != nil || !strings.EqualFold(u.Hostname(), "github.com") {
			return ""
		}
		repo = strings.TrimPrefix(u.Path, "/")
	}
	repo = strings.TrimSuffix(strings.TrimSuffix(repo, "/"), ".git")
	if !repositoryPattern.MatchString(repo) {
		return ""
	}
	return repo
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
