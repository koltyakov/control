// Package buildinfo identifies the running binary, independently of protocol version.
package buildinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"runtime"
	"sync"
)

// Build metadata is set by the release/development builder using -ldflags.
var Version = "0.1.0"
var ReleaseRepo = "koltyakov/control"
var BuildTime string

type Info struct {
	Version     string `json:"version"`
	ReleaseRepo string `json:"releaseRepo,omitempty"`
	BuildTime   string `json:"buildTime,omitempty"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	SHA256      string `json:"sha256,omitempty"`
}

var once sync.Once
var current Info

func Current() Info {
	once.Do(func() {
		current = Info{Version: Version, ReleaseRepo: ReleaseRepo, BuildTime: BuildTime, OS: runtime.GOOS, Arch: runtime.GOARCH}
		path, err := os.Executable()
		if err != nil {
			return
		}
		f, err := os.Open(path)
		if err != nil {
			return
		}
		defer f.Close()
		hash := sha256.New()
		if _, err = io.Copy(hash, f); err == nil {
			current.SHA256 = hex.EncodeToString(hash.Sum(nil))
		}
	})
	return current
}
