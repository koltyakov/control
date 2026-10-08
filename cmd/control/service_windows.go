package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/koltyakov/control/internal/installation"
	"github.com/koltyakov/control/internal/processutil"
	"golang.org/x/sys/windows/svc"
)

func runPlatformService(ctx context.Context, args []string) (bool, error) {
	if len(args) == 0 || (args[0] != "__service" && args[0] != "__user" && args[0] != "__user-launch") {
		return false, nil
	}
	count := 2
	if args[0] == "__user-launch" {
		count = 3
	}
	if len(args) != count || !filepath.IsAbs(args[1]) {
		return true, errors.New("Windows service requires an absolute node configuration path")
	}
	if args[0] == "__user-launch" && !filepath.IsAbs(args[2]) {
		return true, errors.New("user startup requires an absolute installed CLI path")
	}
	config := args[1]
	log, err := os.OpenFile(filepath.Join(filepath.Dir(config), "node.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return true, err
	}
	defer log.Close()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = log, log
	defer func() { os.Stdout, os.Stderr = stdout, stderr }()
	if args[0] == "__user-launch" {
		// This process has the GUI subsystem, so Task Scheduler never allocates
		// a console. Run the original, unmodified CLI without a console too and
		// wait for it so task state and failure recovery follow the supervisor.
		cmd := exec.CommandContext(ctx, args[2], "__user", config)
		processutil.HideWindow(cmd)
		cmd.Stdout, cmd.Stderr = log, log
		return true, cmd.Run()
	}
	if args[0] == "__user" {
		return true, run(ctx, []string{"--token", "", "node", "--config", config})
	}
	return true, svc.Run(installation.WindowsServiceName(config), &windowsNodeService{
		ctx: ctx,
		run: func(ctx context.Context) error {
			return run(ctx, []string{"--token", "", "node", "--config", config})
		},
	})
}

type windowsNodeService struct {
	ctx context.Context
	run func(context.Context) error
}

func (s *windowsNodeService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	statuses <- svc.Status{State: svc.StartPending, WaitHint: 10000}
	done := make(chan error, 1)
	go func() { done <- s.run(ctx) }()
	status := svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	statuses <- status
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	stopping := false
	parentDone := s.ctx.Done()
	stop := func() {
		if !stopping {
			stopping = true
			cancel()
			status = svc.Status{State: svc.StopPending, CheckPoint: 1, WaitHint: 20000}
			statuses <- status
		}
	}
	for {
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintln(os.Stderr, "control service:", err)
				return true, 1
			}
			return false, 0
		case request, ok := <-requests:
			if !ok {
				requests = nil
				stop()
				continue
			}
			switch request.Cmd {
			case svc.Interrogate:
				statuses <- status
			case svc.Stop, svc.Shutdown:
				stop()
			}
		case <-parentDone:
			parentDone = nil
			stop()
		case <-ticker.C:
			if stopping {
				status.CheckPoint++
				statuses <- status
			}
		}
	}
}
