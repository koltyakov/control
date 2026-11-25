package client

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/shellstream"
)

// Shell is one interactive session. One reader may run alongside Write,
// Resize, and Close. Only an explicit worker exit record produces io.EOF.
type Shell struct {
	conn   net.Conn
	stop   func() bool
	close  func() error
	exit   atomic.Pointer[int]
	input  sync.Mutex
	writer shellstream.Writer

	output  [shellstream.MaxData]byte
	pending []byte
	readErr error
}

// OpenShell opens a foreground session as the target's worker account.
// No shell state is persisted and an interrupted open is never retried.
func (c Client) OpenShell(ctx context.Context, target string, request model.ShellRequest) (*Shell, error) {
	if target == "" {
		return nil, errors.New("shell requires a target machine")
	}
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if c.routing == nil || !c.routing.cfg.AccountRouting {
		return nil, errors.New("shell requires account-client routing; omit --api and CONTROL_API")
	}
	ctx, cancel := c.lifetimeContext(ctx)
	shell, err := c.openShell(ctx, target, request, cancel)
	if err != nil {
		cancel()
	}
	return shell, err
}

func (c Client) openShell(ctx context.Context, target string, request model.ShellRequest, cancel context.CancelFunc) (*Shell, error) {
	peer, err := c.backend(ctx)
	if err != nil {
		return nil, err
	}
	destination, err := peer.Lookup(ctx, target)
	if err != nil {
		return nil, err
	}
	if !destination.InstructionDelegation {
		return nil, errors.New("target does not support instruction-bound delegation; upgrade it before opening a shell")
	}
	if destination.Disabled || destination.ControlPending {
		return nil, errors.New("target is disabled or has a pending control operation")
	}
	var description struct {
		Shell model.ShellDescription `json:"shell"`
	}
	if err := peerCall(ctx, peer, destination.ID, "node.describe", nil, &description); err != nil {
		return nil, err
	}
	if description.Shell.Protocol != model.ShellProtocol || description.Shell.Method != model.ShellOpenMethod {
		return nil, errors.New("target does not support this shell protocol; upgrade the worker")
	}
	if !description.Shell.Enabled {
		return nil, errors.New("interactive shells are disabled on the worker; set shellEnabled in its node configuration")
	}
	conn, result, err := openPeerRPC(ctx, peer, destination.ID, model.ShellOpenMethod, request)
	if err != nil {
		return nil, err
	}
	var ack struct {
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(result, &ack); err != nil || ack.Protocol != model.ShellProtocol {
		_ = conn.Close()
		return nil, errors.New("invalid worker shell acknowledgement")
	}
	s := &Shell{conn: conn}
	s.close = sync.OnceValue(func() error {
		cancel()
		_ = conn.SetDeadline(time.Now())
		return conn.Close()
	})
	s.stop = context.AfterFunc(ctx, func() { _ = s.close() })
	if err := ctx.Err(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Shell) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if s.readErr != nil {
		return 0, s.readErr
	}
	if len(s.pending) == 0 {
		kind, data, err := shellstream.Read(s.conn, &s.output)
		switch {
		case err != nil:
			// Do not wrap EOF itself: io.Copy treats it as a clean close.
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			s.readErr = fmt.Errorf("remote shell ended without an exit status: %w", err)
		case kind == shellstream.Exit:
			code := int(binary.BigEndian.Uint32(data))
			s.exit.Store(&code)
			s.readErr = io.EOF
		case kind != shellstream.Output:
			s.readErr = errors.New("unexpected record from remote shell")
		default:
			s.pending = data
		}
		if s.readErr != nil {
			return 0, s.readErr
		}
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *Shell) Write(p []byte) (int, error) {
	s.input.Lock()
	defer s.input.Unlock()
	written := 0
	for len(p) > 0 {
		n := min(len(p), shellstream.MaxData)
		if err := s.writer.Write(s.conn, shellstream.Input, p[:n]); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

func (s *Shell) Resize(cols, rows uint16) error {
	if !model.ValidShellDimensions(cols, rows) {
		return errors.New("terminal dimensions must be 1..32767")
	}
	var data [4]byte
	binary.BigEndian.PutUint16(data[:2], cols)
	binary.BigEndian.PutUint16(data[2:], rows)
	s.input.Lock()
	defer s.input.Unlock()
	return s.writer.Write(s.conn, shellstream.Resize, data[:])
}

func (s *Shell) ExitCode() (int, bool) {
	if code := s.exit.Load(); code != nil {
		return *code, true
	}
	return 0, false
}

func (s *Shell) Close() error {
	s.stop()
	return s.close()
}
