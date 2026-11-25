package shellpty

import (
	"bytes"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder drains a Terminal in the background, as the shell server does.
type recorder struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	err  error
	done chan struct{}
}

func record(r io.Reader) *recorder {
	rec := &recorder{done: make(chan struct{})}
	go func() {
		defer close(rec.done)
		chunk := make([]byte, 32*1024)
		for {
			n, err := r.Read(chunk)
			rec.mu.Lock()
			rec.buf.Write(chunk[:n])
			if err != nil {
				rec.err = err
			}
			rec.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	return rec
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buf.String()
}

// waitFor polls until output contains want, then returns the output.
func (r *recorder) waitFor(t *testing.T, want string) string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if out := r.String(); strings.Contains(out, want) {
			return out
		}
		select {
		case <-r.done:
			if out := r.String(); strings.Contains(out, want) {
				return out
			}
			t.Fatalf("output ended (%v) before %q:\n%s", r.err, want, r.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatalf("timed out waiting for %q:\n%s", want, r.String())
	return ""
}

// eof waits for the reader to finish and requires a clean io.EOF.
func (r *recorder) eof(t *testing.T) string {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("output did not reach EOF:\n%s", r.String())
	}
	if !errors.Is(r.err, io.EOF) {
		t.Fatalf("read ended with %v, want io.EOF", r.err)
	}
	return r.String()
}

// ended waits for the reader to finish with any error.
func (r *recorder) ended(t *testing.T) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(20 * time.Second):
		t.Fatalf("reads did not finish:\n%s", r.String())
	}
}

func waitResult(t *testing.T, term *Terminal) (int, error) {
	t.Helper()
	type result struct {
		code int
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		code, err := term.Wait()
		ch <- result{code, err}
	}()
	select {
	case res := <-ch:
		return res.code, res.err
	case <-time.After(20 * time.Second):
		t.Fatal("Wait did not return")
		return 0, nil
	}
}

func requireShell(t *testing.T, name string) {
	t.Helper()
	if !slices.Contains(Available(), name) {
		t.Skipf("%s is not installed", name)
	}
}

func closeWithin(t *testing.T, term *Terminal) {
	t.Helper()
	start := time.Now()
	_ = term.Close()
	if elapsed := time.Since(start); elapsed > closeTimeout+2*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
}
