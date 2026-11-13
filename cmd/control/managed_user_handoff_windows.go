package main

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/installation"
	"golang.org/x/sys/windows"
)

const userHandoffPipePrefix = `\\.\pipe\ControlUpdate-`

func listenUserManagedHandoff(id string) (net.Listener, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	// No inherited or cross-user grants. go-winio also rejects remote clients
	// and atomically refuses a pipe name that already has a first instance.
	return winio.ListenPipe(userHandoffPipePrefix+id, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + user.User.Sid.String() + ")",
		InputBufferSize:    64 << 10, OutputBufferSize: 2048,
	})
}

// Scheduler jobs deny breakaway and can keep the original task Running while
// any descendant lives. A separate, triggerless limited-user task avoids both.
// An ephemeral, current-user-only named pipe authenticates a random capability
// token before sending any bounded prepare/commit exchange, never work.
func prepareUserManagedHandoff(ctx context.Context, request windowsHandoffRequest) (handoff managedHandoff, err error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	listener, err := listenUserManagedHandoff(request.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = listener.Close() }()
	deadline, _ := ctx.Deadline()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	token := identity.NewID() + identity.NewID()
	name := "ControlUpdate-" + request.ID
	// Cleanup also covers a registration accepted just before a launch error.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel()
		if cleanupErr := installation.RemoveUpdateUserHelper(cleanup, name); cleanupErr != nil {
			if handoff != nil {
				handoff.abort()
				handoff = nil
			}
			err = errors.Join(err, fmt.Errorf("remove temporary updater task: %w", cleanupErr))
		}
	}()
	if err = installation.StartUpdateUserHelper(ctx, name, request.Next.Path, []string{"__update-handoff", listener.Addr().String(), token}); err != nil {
		return nil, err
	}
	for {
		conn, err := listener.Accept()
		if err != nil {
			return nil, err
		}
		if err = conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, err
		}
		reader := bufio.NewReader(io.LimitReader(conn, 2048))
		line, err := reader.ReadString('\n')
		if err != nil || subtle.ConstantTimeCompare([]byte(strings.TrimSuffix(line, "\n")), []byte(token)) != 1 {
			_ = conn.Close()
			continue
		}
		if err = json.NewEncoder(conn).Encode(request); err == nil {
			line, err = reader.ReadString('\n')
			if err == nil && line != "ready\n" {
				err = fmt.Errorf("prepare temporary User updater: %s", strings.TrimSpace(line))
			}
		}
		if err != nil {
			_ = conn.Close() // EOF never authorizes a switch.
			return nil, err
		}
		if err = conn.SetDeadline(time.Time{}); err != nil {
			_ = conn.Close()
			return nil, err
		}
		return &windowsManagedHandoff{input: conn}, nil
	}
}

func runUserManagedHandoff(ctx context.Context, endpoint []string) error {
	if len(endpoint) != 2 || len(endpoint[1]) != 64 {
		return errors.New("invalid User updater endpoint")
	}
	id := strings.TrimPrefix(endpoint[0], userHandoffPipePrefix)
	if !strings.HasPrefix(endpoint[0], userHandoffPipePrefix) || len(id) != 32 {
		return errors.New("user updater requires a local Control pipe")
	}
	if _, err := hex.DecodeString(id); err != nil {
		return errors.New("invalid User updater pipe identifier")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	conn, err := winio.DialPipeContext(dialCtx, endpoint[0])
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	if err = conn.SetDeadline(time.Now().Add(2 * time.Minute)); err != nil {
		return err
	}
	if _, err = fmt.Fprintln(conn, endpoint[1]); err != nil {
		return err
	}
	if err = runManagedHandoffIO(ctx, conn, conn); err != nil {
		_, _ = fmt.Fprintf(conn, "error: %.1024s\n", err.Error())
	}
	return err
}
