// Package commandexec owns the ordinary descendants of one-shot commands.
// Persistent application adapters deliberately do not use this package.
package commandexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

const helperMode = "__control_command_v1"

type outcome struct {
	ExitCode int       `json:"exitCode"`
	Error    string    `json:"error,omitempty"`
	Usage    *measured `json:"usage,omitempty"`
}

// The private re-exec entry point also works in test executables. In Control it
// runs before node/profile initialization, never loads credentials, and has no
// authority beyond the invoking OS account. The gate prevents execution until
// the parent has established process ownership.
func init() {
	if len(os.Args) < 6 || os.Args[1] != helperMode {
		return
	}
	os.Exit(runHelper(os.Args[2:]))
}

func runHelper(args []string) int {
	status, gate, err := helperFiles(args[0], args[1])
	if err != nil {
		return 125
	}
	defer func() { _ = status.Close() }()
	defer func() { _ = gate.Close() }()
	var ready [1]byte
	_, err = io.ReadFull(gate, ready[:])
	if err != nil || ready[0] != 1 {
		return 125
	}
	parentGone := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, gate)
		close(parentGone)
		terminateHelper()
		os.Exit(125)
	}()
	// The requested command receives neither the gate nor the status channel.
	cmd := exec.Command(args[2], args[4:]...)
	cmd.Args = append([]string(nil), args[3:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	hideChild(cmd)
	result := outcome{ExitCode: -1}
	err = cmd.Run()
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
		result.Usage = programUsage(cmd.ProcessState)
	}
	if err != nil {
		result.Error = err.Error()
		if len(result.Error) > 4096 {
			result.Error = result.Error[:4096]
		}
	}
	if json.NewEncoder(status).Encode(result) != nil {
		return 125
	}
	// Remain alive until the parent cleans up the group/job. This also avoids
	// platform differences when signalling a group containing only zombies.
	<-parentGone
	return 125
}

// Run returns the requested program's exit code, not the helper's. The helper
// keeps its process identity reserved until the parent terminates its group/job
// and calls Wait. A status-read failure never authorizes another execution.
func Run(ctx context.Context, command *exec.Cmd) (int, error) {
	exit, _, err := RunWithUsage(ctx, command)
	return exit, err
}

// RunWithUsage is Run plus the invocation's resource accounting, or nil when
// the requested program never ran or nothing could be measured. Measurement
// failures leave fields unknown and never change the exit code or error.
func RunWithUsage(ctx context.Context, command *exec.Cmd) (int, *model.CommandUsage, error) {
	if err := ctx.Err(); err != nil {
		return -1, nil, err
	}
	if command.Err != nil {
		return -1, nil, command.Err
	}
	executable, err := os.Executable()
	if err != nil {
		return -1, nil, err
	}
	status, statusWriter, err := os.Pipe()
	if err != nil {
		return -1, nil, err
	}
	defer func() { _ = status.Close(); _ = statusWriter.Close() }()
	gateReader, gate, err := os.Pipe()
	if err != nil {
		return -1, nil, err
	}
	defer func() { _ = gateReader.Close(); _ = gate.Close() }()
	helper := exec.CommandContext(ctx, executable)
	helper.Dir, helper.Env = command.Dir, command.Env
	helper.Stdin = command.Stdin
	helper.WaitDelay = command.WaitDelay
	output, err := newOutput(helper, command)
	if err != nil {
		return -1, nil, err
	}
	defer output.close()
	owner, handles, err := prepare(helper, statusWriter, gateReader)
	if err != nil {
		return -1, nil, err
	}
	defer owner.close()
	args := command.Args
	if len(args) == 0 {
		args = []string{command.Path}
	}
	helper.Args = append([]string{executable, helperMode, handles[0], handles[1], command.Path}, args...)
	if err = helper.Start(); err != nil {
		return -1, nil, err
	}
	output.start()
	_ = statusWriter.Close()
	_ = gateReader.Close()
	var started time.Time
	if err = owner.adopt(helper.Process); err == nil {
		started = time.Now()
		_, err = gate.Write([]byte{1})
	}
	if err != nil {
		if owner.terminate(helper.Process) != nil {
			_ = gate.Close()
			_ = helper.Process.Kill()
		}
		_ = helper.Wait()
		_ = output.wait(command.WaitDelay)
		return -1, nil, fmt.Errorf("establish command ownership: %w", err)
	}
	result := outcome{ExitCode: -1}
	readErr := json.NewDecoder(io.LimitReader(status, 16<<10)).Decode(&result)
	wall := time.Since(started)
	// Never Wait before terminating the group: a live or unreaped helper PID
	// prevents its process-group identity being reused for unrelated processes.
	cleanupErr := owner.terminate(helper.Process)
	if cleanupErr != nil {
		_ = gate.Close()
		_ = helper.Process.Kill()
	}
	waitErr := helper.Wait()
	// The job (Windows) is still open here; owner.close runs on return.
	reading := owner.usage(helper.ProcessState)
	if reading == nil && readErr == nil {
		reading = result.Usage
	}
	usage := commandUsage(wall, reading)
	outputErr := output.wait(command.WaitDelay)
	if err := ctx.Err(); err != nil {
		return result.ExitCode, usage, err
	}
	if readErr != nil {
		return -1, usage, errors.Join(fmt.Errorf("command status: %w", readErr), cleanupErr, waitErr)
	}
	// A signalled helper is expected after group/job cleanup. WaitDelay is not:
	// an escaped descendant may still hold output open, leaving it incomplete.
	if outputErr != nil {
		return result.ExitCode, usage, errors.Join(outputErr, cleanupErr)
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return result.ExitCode, usage, errors.Join(waitErr, cleanupErr)
	}
	if result.Error != "" {
		return result.ExitCode, usage, errors.Join(errors.New(result.Error), cleanupErr)
	}
	if result.ExitCode != 0 {
		return result.ExitCode, usage, errors.Join(fmt.Errorf("exit status %d", result.ExitCode), cleanupErr)
	}
	return result.ExitCode, usage, cleanupErr
}

type outputPipe struct {
	reader *os.File
	writer *os.File
	dest   io.Writer
	done   chan error
}

type outputPipes [2]outputPipe

type lockedWriter struct {
	mu     *sync.Mutex
	writer io.Writer
}

func (w lockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writer.Write(data)
}

func newOutput(helper, command *exec.Cmd) (*outputPipes, error) {
	pipes := &outputPipes{}
	destinations := [2]io.Writer{command.Stdout, command.Stderr}
	// Preserve os/exec's guarantee for a shared comparable output writer while
	// retaining independent flow control for unrelated destinations.
	if command.Stdout != nil && reflect.TypeOf(command.Stdout).Comparable() && command.Stdout == command.Stderr {
		mu := &sync.Mutex{}
		destinations = [2]io.Writer{lockedWriter{mu, command.Stdout}, lockedWriter{mu, command.Stderr}}
	}
	for i, dest := range destinations {
		reader, writer, err := os.Pipe()
		if err != nil {
			pipes.close()
			return nil, err
		}
		if dest == nil {
			dest = io.Discard
		}
		pipes[i] = outputPipe{reader: reader, writer: writer, dest: dest, done: make(chan error, 1)}
	}
	helper.Stdout, helper.Stderr = pipes[0].writer, pipes[1].writer
	return pipes, nil
}

func (pipes *outputPipes) start() {
	for i := range pipes {
		pipe := &pipes[i]
		_ = pipe.writer.Close()
		go func() {
			defer func() { _ = pipe.reader.Close() }()
			_, err := io.Copy(pipe.dest, pipe.reader)
			pipe.done <- err
		}()
	}
}

func (pipes *outputPipes) wait(delay time.Duration) error {
	if delay <= 0 {
		delay = 2 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var result error
	for i := range pipes {
		select {
		case err := <-pipes[i].done:
			result = errors.Join(result, err)
		case <-timer.C:
			pipes.close()
			for j := i; j < len(pipes); j++ {
				<-pipes[j].done
			}
			return errors.Join(result, exec.ErrWaitDelay)
		}
	}
	return result
}

func (pipes *outputPipes) close() {
	for _, pipe := range pipes {
		if pipe.reader != nil {
			_ = pipe.reader.Close()
			_ = pipe.writer.Close()
		}
	}
}
