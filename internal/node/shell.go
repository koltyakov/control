package node

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/processutil"
	"github.com/koltyakov/control/internal/shellpty"
	"github.com/koltyakov/control/internal/shellstream"
	"github.com/koltyakov/control/internal/wire"
)

func (cfg Config) shellStartDir(runAs string) (string, error) {
	if cfg.HybridSystem || runAs != "user" {
		return cfg.WorkDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve worker user's home directory for shell: %w", err)
	}
	return home, nil
}

func (n *Node) serveShell(ctx context.Context, caller string, stream net.Conn, args json.RawMessage) {
	run := func() error {
		if err := n.authorize(ctx, caller, model.ShellOpenMethod); err != nil {
			return err
		}
		if !n.Config.ShellEnabled {
			return errors.New("interactive shells are disabled on the worker")
		}
		var request model.ShellRequest
		if err := json.Unmarshal(args, &request); err != nil {
			return err
		}
		if err := request.Validate(); err != nil {
			return err
		}
		return n.startShellStream(ctx, caller, stream, request)
	}
	if err := run(); err != nil {
		_ = wire.WriteResponse(stream, model.ErrorResponse(err))
	}
}

// Only errors before the acknowledgement can be returned to serveShell.
func (n *Node) startShellStream(ctx context.Context, caller string, stream net.Conn, request model.ShellRequest) error {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { _ = stream.SetDeadline(time.Now()) })
	// Read the peer independently of terminal writes. A child waiting on
	// output or not reading input must not hide a disconnect.
	inputs := make(chan shellInput, 64)
	readerDone := make(chan struct{})
	var acceptingInput atomic.Bool
	go func() {
		defer close(readerDone)
		defer cancel()
		readShellInputs(ctx, stream, inputs, &acceptingInput)
	}()
	defer func() {
		stop()
		cancel()
		_ = stream.SetReadDeadline(time.Now())
		<-readerDone
	}()
	workCtx, release, err := n.enterWork(ctx)
	if err != nil {
		return err
	}
	defer release()
	releaseInvocation, err := n.reserveLeaseSensitiveStream(workCtx)
	if err != nil {
		return err
	}
	defer releaseInvocation()
	if !n.executionPool.tryAcquire() {
		return errors.New("worker execution slots are busy; try opening a shell again when one is free")
	}
	defer n.executionPool.release()
	if err := workCtx.Err(); err != nil {
		return err
	}
	dir, err := n.Config.shellStartDir(processutil.RunAs())
	if err != nil {
		return err
	}
	activity := n.beginActivity(workCtx, "operation", model.ShellOpenMethod, n.executionOwner(workCtx, caller), caller)
	activity.phase("starting")
	terminal, err := shellpty.Start(workCtx, request.Shell, request.Cols, request.Rows, request.Term, dir)
	if err != nil {
		activity.finish(err)
		return err
	}
	var inputDone chan struct{}
	defer func() {
		cancel()
		_ = terminal.Close()
		if inputDone != nil {
			<-inputDone
		}
	}()
	acceptingInput.Store(true)
	if err = wire.WriteResponse(stream, model.Response{Result: model.JSON(map[string]string{"protocol": model.ShellProtocol})}); err == nil {
		activity.phase("interactive")
		inputDone = make(chan struct{})
		go func() {
			defer close(inputDone)
			applyShellInputs(workCtx, terminal, inputs)
		}()
		err = writeShellOutput(workCtx, terminal, stream)
	}
	activity.finish(err)
	return nil
}

type shellInput struct {
	data       []byte
	cols, rows uint16
}

func readShellInputs(ctx context.Context, stream net.Conn, inputs chan<- shellInput, accepting *atomic.Bool) {
	defer close(inputs)
	var scratch [shellstream.MaxData]byte
	for ctx.Err() == nil {
		kind, data, err := shellstream.Read(stream, &scratch)
		if err != nil || !accepting.Load() {
			return
		}
		var input shellInput
		switch kind {
		case shellstream.Input:
			input.data = slices.Clone(data)
		case shellstream.Resize:
			input.cols, input.rows = binary.BigEndian.Uint16(data[:2]), binary.BigEndian.Uint16(data[2:])
			if !model.ValidShellDimensions(input.cols, input.rows) {
				return
			}
		default:
			return
		}
		select {
		case inputs <- input:
		default:
			// Unconsumed input is limited to 1 MiB. Fail closed rather than
			// blocking the network reader or dropping keystrokes.
			return
		}
	}
}

func applyShellInputs(ctx context.Context, terminal *shellpty.Terminal, inputs <-chan shellInput) {
	for input := range inputs {
		if ctx.Err() != nil {
			return
		}
		var err error
		if input.data != nil {
			_, err = terminal.Write(input.data)
		} else {
			err = terminal.Resize(input.cols, input.rows)
		}
		if err != nil {
			// The shell may have exited while this input was in flight.
			// Let the output pump deliver the remaining bytes and status.
			return
		}
	}
}

type shellOutput interface {
	io.ReadCloser
	Done() <-chan struct{}
	Wait() (int, error)
}

func writeShellOutput(ctx context.Context, terminal shellOutput, stream net.Conn) error {
	// Some shells can start programs outside the owned process group or
	// session. An inherited terminal handle must neither make cancellation
	// wait for EOF nor leave a dead shell attached indefinitely.
	pumpCtx, stop := context.WithCancel(ctx)
	guardDone := make(chan struct{})
	go func() {
		defer close(guardDone)
		select {
		case <-pumpCtx.Done():
			if ctx.Err() == nil {
				return
			}
		case <-terminal.Done():
			timer := time.NewTimer(30 * time.Second)
			defer timer.Stop()
			select {
			case <-pumpCtx.Done():
				if ctx.Err() == nil {
					return
				}
			case <-timer.C:
			}
		}
		_ = terminal.Close()
	}()
	defer func() { stop(); <-guardDone }()

	var writer shellstream.Writer
	send := func(kind byte, data []byte) error {
		if err := stream.SetWriteDeadline(time.Now().Add(30 * time.Second)); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return writer.Write(stream, kind, data)
	}
	var scratch [shellstream.MaxData]byte
	for {
		size, err := terminal.Read(scratch[:])
		if size != 0 {
			if writeErr := send(shellstream.Output, scratch[:size]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-terminal.Done():
	}
	code, err := terminal.Wait()
	if err != nil {
		return err
	}
	var status [4]byte
	binary.BigEndian.PutUint32(status[:], uint32(code))
	return send(shellstream.Exit, status[:])
}
