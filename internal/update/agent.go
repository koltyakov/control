package update

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/store"
)

type AgentOptions struct {
	Gateway, Token, Dir string
	Software            buildinfo.Info
	Busy                func() bool
	Pause               func() bool
	Resume              func()
	Report              func(context.Context, Status) error
	Apply               func(string) error
}

type agentCommand struct {
	kind    string
	command Command
}
type Agent struct {
	options     AgentOptions
	commands    chan agentCommand
	mu          sync.Mutex
	status      Status
	target      Command
	lease       time.Time
	staged      string
	nextAttempt time.Time
}

func NewAgent(options AgentOptions) (*Agent, error) {
	a := &Agent{options: options, commands: make(chan agentCommand, 32), status: Status{Software: options.Software, State: "idle"}}
	var saved Command
	if err := store.Read(a.marker(), &saved); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if saved.ID != "" && time.Now().Before(saved.LeaseUntil) && options.Pause() {
		a.target, a.lease = saved, saved.LeaseUntil
		a.staged = a.stagePath(saved.Asset)
		a.status.ID, a.status.State, a.status.Paused = saved.ID, "ready", true
		a.status.LeaseUntil = saved.LeaseUntil
		if Matches(options.Software, saved.Asset) {
			a.status.State = "applied"
		}
	}
	return a, nil
}

func (a *Agent) marker() string { return filepath.Join(a.options.Dir, "update-maintenance.json") }
func (a *Agent) stagePath(asset Asset) string {
	return filepath.Join(a.options.Dir, "updates", asset.SHA256, asset.File)
}

// Receive is called only for messages authenticated as originating at the gateway.
func (a *Agent) Receive(kind string, data []byte) {
	var command Command
	if json.Unmarshal(data, &command) != nil || !ValidDigest(command.ID) {
		return
	}
	select {
	case a.commands <- agentCommand{kind, command}:
	default:
	}
}

func (a *Agent) Snapshot() Status {
	a.mu.Lock()
	s := a.status
	a.mu.Unlock()
	s.Busy, s.SeenAt = a.options.Busy(), time.Now().UTC()
	return s
}

func (a *Agent) set(state string, err error) {
	a.mu.Lock()
	a.status.State, a.status.Error, a.status.ID = state, ShortError(err), a.target.ID
	a.mu.Unlock()
}

func (a *Agent) report(ctx context.Context) { _ = a.options.Report(ctx, a.Snapshot()) }

func (a *Agent) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	a.report(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !a.lease.IsZero() && time.Now().After(a.lease) {
				a.resume()
			}
		case command := <-a.commands:
			a.process(ctx, command)
		}
		a.report(ctx)
	}
}

func (a *Agent) resume() {
	_ = os.Remove(a.marker())
	a.lease = time.Time{}
	a.options.Resume()
	a.mu.Lock()
	a.status.Paused = false
	a.status.LeaseUntil = time.Time{}
	a.mu.Unlock()
	if Matches(a.options.Software, a.target.Asset) {
		a.set("applied", nil)
	} else if a.staged != "" {
		a.set("staged", nil)
	} else {
		a.set("idle", nil)
	}
}

func (a *Agent) process(ctx context.Context, message agentCommand) {
	command := message.command
	switch message.kind {
	case "update.offer":
		if a.target.ID == command.ID && (a.Snapshot().State != "error" || time.Now().Before(a.nextAttempt)) {
			return
		}
		m := Manifest{Version: command.Version, Assets: []Asset{command.Asset}}
		if err := m.Validate(); err != nil {
			return
		}
		if command.Asset.OS != a.options.Software.OS || command.Asset.Arch != a.options.Software.Arch {
			return
		}
		a.resume()
		a.target, a.staged = command, ""
		if Matches(a.options.Software, command.Asset) {
			a.set("applied", nil)
			return
		}
		a.set("downloading", nil)
		a.report(ctx)
		path := a.stagePath(command.Asset)
		err := Download(ctx, &http.Client{Timeout: 2 * time.Minute}, a.options.Gateway+"/v1/updates/blobs/"+command.Asset.SHA256, a.options.Token, path, command.Asset)
		if err == nil {
			err = ValidateExecutable(ctx, path, command.Version, command.Asset)
		}
		if err != nil {
			a.nextAttempt = time.Now().Add(30 * time.Second)
			a.set("error", err)
			return
		}
		a.staged = path
		a.set("staged", nil)
	case "update.prepare":
		if command.ID != a.target.ID || (a.staged == "" && !Matches(a.options.Software, a.target.Asset)) {
			return
		}
		if command.LeaseUntil.Before(time.Now()) || command.LeaseUntil.After(time.Now().Add(5*time.Minute)) {
			return
		}
		if !a.options.Pause() {
			a.set("busy", nil)
			return
		}
		a.lease = command.LeaseUntil
		a.target.LeaseUntil = command.LeaseUntil
		if err := store.Write(a.marker(), a.target); err != nil {
			a.resume()
			a.set("error", err)
			return
		}
		a.mu.Lock()
		a.status.Paused = true
		a.status.LeaseUntil = a.lease
		a.mu.Unlock()
		if Matches(a.options.Software, a.target.Asset) {
			a.set("applied", nil)
		} else {
			a.set("ready", nil)
		}
	case "update.resume":
		if command.ID == a.target.ID {
			a.resume()
		}
	case "update.commit":
		if command.ID != a.target.ID {
			return
		}
		if Matches(a.options.Software, a.target.Asset) {
			a.set("applied", nil)
			return
		}
		if a.lease.IsZero() || time.Now().After(a.lease) || !a.options.Pause() {
			a.set("busy", nil)
			return
		}
		if a.options.Apply == nil {
			a.resume()
			a.set("error", errors.New("managed updates unavailable"))
			return
		}
		if err := Verify(a.staged, a.target.Asset); err != nil {
			a.resume()
			a.set("error", err)
			return
		}
		a.set("restarting", nil)
		a.report(ctx)
		if err := a.options.Apply(a.staged); err != nil {
			a.resume()
			a.set("error", err)
		}
	}
}
