package update

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/koltyakov/control/internal/buildinfo"
	"github.com/koltyakov/control/internal/workgate"
)

func proxyUpdaterFixture(t *testing.T) (*Updater, UpdaterOptions, Command, *workgate.Gate) {
	t.Helper()
	gate := workgate.New()
	info := buildinfo.Info{Version: "selected", OS: "linux", Arch: "amd64", SHA256: strings.Repeat("b", 64)}
	options := UpdaterOptions{Dir: t.TempDir(), Software: info,
		Busy: func() bool { n, _ := gate.State(); return n != 0 }, Pause: gate.PauseIfIdle, Resume: gate.Resume,
		ProxyWorker: strings.Repeat("a", 64), ProxyURL: "http://10.0.0.1:3128",
		ParkGateway: func(time.Time) bool { return true }, ResumeGateway: func() {},
		Report: func(context.Context, Status) error { return nil },
		Apply:  func(string) error { t.Fatal("a proxy hold executed a child update"); return nil },
	}
	a, err := NewUpdater(options)
	if err != nil {
		t.Fatal(err)
	}
	c := Command{ID: strings.Repeat("c", 64), Version: info.Version,
		Asset:    Asset{OS: info.OS, Arch: info.Arch, File: AssetName(info.OS, info.Arch), Size: 1, SHA256: info.SHA256},
		ProxyFor: options.ProxyWorker, ProxyURL: options.ProxyURL, LeaseUntil: time.Now().Add(2 * time.Minute)}
	offer := c
	offer.ProxyFor, offer.ProxyURL = "", ""
	a.process(t.Context(), queuedCommand{"update.offer", offer})
	return a, options, c, gate
}

func TestProxyHoldReservesRestoresAndResumesBeforeChildUpdate(t *testing.T) {
	a, options, c, gate := proxyUpdaterFixture(t)
	a.process(t.Context(), queuedCommand{"update.proxy-prepare", c})
	if s := a.Snapshot(); !s.ProxyDrain || !s.Paused || s.ProxyFor != c.ProxyFor {
		t.Fatal("child did not acknowledge its proxy hold", s)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	if release, err := gate.Enter(ctx); err == nil {
		release()
		t.Fatal("child admitted new work during proxy hold")
	}
	// The ordinary update protocol cannot steal this reservation, even when
	// the child has a different staged binary.
	a.options.Software.Version = "older"
	a.process(t.Context(), queuedCommand{"update.prepare", c})
	a.process(t.Context(), queuedCommand{"update.commit", c})
	restarted, err := NewUpdater(options)
	if err != nil {
		t.Fatal(err)
	}
	if s := restarted.Snapshot(); !s.Paused || s.ProxyFor != c.ProxyFor {
		t.Fatal("lost the durable child reservation on restart", s)
	}
	parks, resumes := 0, 0
	restarted.options.ParkGateway = func(time.Time) bool { parks++; return true }
	restarted.options.ResumeGateway = func() { resumes++ }
	c.ParkUntil = time.Now().Add(20 * time.Second)
	restarted.process(t.Context(), queuedCommand{"update.proxy-park", c})
	c.ParkUntil = c.ParkUntil.Add(time.Second)
	restarted.process(t.Context(), queuedCommand{"update.proxy-park", c})
	if parks != 1 {
		t.Fatal("duplicate park extended the outage", parks)
	}
	restarted.process(t.Context(), queuedCommand{"update.resume", c})
	if s := restarted.Snapshot(); s.Paused || s.ProxyFor != "" || !s.ParkUntil.IsZero() || resumes != 1 {
		t.Fatal("child did not resume cleanly", s, resumes)
	}
	if _, err := os.Stat(filepath.Join(options.Dir, "update-maintenance.json")); !os.IsNotExist(err) {
		t.Fatal("child hold marker retained", err)
	}
}

func TestProxyHoldRejectsBusyMismatchedAndUncertainParking(t *testing.T) {
	for _, name := range []string{"busy", "wrong-worker", "wrong-url", "expired", "far-future", "write-failure", "report-failure", "live-traffic", "park-too-long", "wrong-deployment"} {
		t.Run(name, func(t *testing.T) {
			a, options, c, gate := proxyUpdaterFixture(t)
			switch name {
			case "busy":
				release, err := gate.Enter(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			case "wrong-worker":
				c.ProxyFor = strings.Repeat("d", 64)
			case "wrong-url":
				c.ProxyURL = "http://10.0.0.2:3128"
			case "expired":
				c.LeaseUntil = time.Now().Add(-time.Second)
			case "far-future":
				c.LeaseUntil = time.Now().Add(6 * time.Minute)
			case "wrong-deployment":
				c.ID = strings.Repeat("d", 64)
			case "write-failure":
				if err := os.Mkdir(a.marker(), 0700); err != nil {
					t.Fatal(err)
				}
			}
			a.process(t.Context(), queuedCommand{"update.proxy-prepare", c})
			parked := false
			a.options.ParkGateway = func(time.Time) bool { parked = true; return name != "live-traffic" }
			if name == "report-failure" {
				a.options.Report = func(context.Context, Status) error { return errors.New("lost acknowledgement") }
			}
			c.ParkUntil = time.Now().Add(20 * time.Second)
			if name == "park-too-long" {
				c.ParkUntil = time.Now().Add(time.Minute)
			}
			a.process(t.Context(), queuedCommand{"update.proxy-park", c})
			if name != "live-traffic" && parked {
				t.Fatal("unsafe park callback ran")
			}
			if !a.Snapshot().ParkUntil.IsZero() {
				t.Fatal("failed park was acknowledged")
			}
			a.resume()
			// Re-enrollment through another proxy cannot restore this hold.
			options.ProxyWorker = strings.Repeat("e", 64)
			if _, err := NewUpdater(options); err != nil && name != "write-failure" {
				t.Fatal(err)
			}
		})
	}
}

func TestProxyHoldExpiresAfterCoordinatorLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a, _, c, gate := proxyUpdaterFixture(t)
		c.LeaseUntil = time.Now().Add(3 * time.Second)
		a.process(t.Context(), queuedCommand{"update.proxy-prepare", c})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { defer close(done); a.Run(ctx) }()
		time.Sleep(5 * time.Second)
		if _, paused := gate.State(); paused || a.Snapshot().ProxyFor != "" {
			t.Fatal("coordinator loss left new work permanently reserved")
		}
		cancel()
		<-done
	})
}

func TestProxyHoldCannotRestoreAfterRouteReplacement(t *testing.T) {
	a, options, c, _ := proxyUpdaterFixture(t)
	a.process(t.Context(), queuedCommand{"update.proxy-prepare", c})
	gate := workgate.New()
	options.Pause, options.Resume = gate.PauseIfIdle, gate.Resume
	options.ProxyWorker = strings.Repeat("d", 64)
	restarted, err := NewUpdater(options)
	if err != nil {
		t.Fatal(err)
	}
	if s := restarted.Snapshot(); s.Paused || s.ProxyFor != "" {
		t.Fatal("old route's hold reserved a replacement route", s)
	}
	if _, paused := gate.State(); paused {
		t.Fatal("replaced route stole work admission")
	}
}
