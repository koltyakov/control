package node

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/koltyakov/control/internal/clipboard"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/upload"
)

func emptyLeaseUpload() upload.Request {
	return upload.Request{Protocol: upload.Protocol, Spec: upload.Spec{ID: "lease-upload", Path: "uploaded.bin", SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"}}
}

// Direct handlers keep unit fixtures independent of transport timing. Closing
// either lifetime interrupts I/O as the peer/local API wrappers do in service.
func localLeaseStream(t *testing.T, n *Node, method string, params any) (net.Conn, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	a, b := net.Pipe()
	closeConns := func() { _ = a.Close(); _ = b.Close() }
	stop := context.AfterFunc(ctx, closeConns)
	stopNode := context.AfterFunc(n.ctx, closeConns)
	done := make(chan struct{})
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer close(done)
		defer func() { _ = a.Close() }()
		if method == upload.OpenMethod {
			n.serveUpload(ctx, n.Identity.ID, a, model.JSON(params))
		} else {
			n.serveClipboard(ctx, n.Identity.ID, a, method, model.JSON(params))
		}
	}()
	t.Cleanup(func() { cancel(); closeConns(); <-done; stop(); stopNode() })
	return b, done
}

func leaseStreamResponse(t *testing.T, conn net.Conn, code string) model.Response {
	t.Helper()
	var response model.Response
	if err := readFrame(conn, &response); err != nil {
		t.Fatal(err)
	}
	if code == "" && response.Error != "" || code != "" && response.Code != code {
		t.Fatal("unexpected stream response", response)
	}
	return response
}

func assertNoLeaseStreams(t *testing.T, n *Node) {
	t.Helper()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.activeInvocations != 0 {
		t.Fatal("leaked invocation reservation", n.activeInvocations)
	}
}

func TestLeaseSensitiveStreamExpiry(t *testing.T) {
	n := localNode(t, nil)
	for _, state := range []string{"unexpired", "expired-task", "expired-acceptance", "expired-idle"} {
		t.Run(state, func(t *testing.T) {
			n.mu.Lock()
			n.lease = &model.Lease{ID: "lease", Owner: "owner", Expires: time.Now().Add(-time.Minute)}
			switch state {
			case "unexpired":
				n.lease.Expires = time.Now().Add(time.Minute)
			case "expired-task":
				n.tasks["busy"] = &model.Task{Spec: model.TaskSpec{LeaseID: "lease"}}
				n.cancels["busy"] = func() {}
			case "expired-acceptance":
				n.accepting["busy"] = &acceptance{leaseID: "lease"}
			}
			n.mu.Unlock()
			release, err := n.reserveLeaseSensitiveStream(t.Context())
			if state == "expired-idle" {
				if err != nil {
					t.Fatal("idle expired lease blocked a new stream", err)
				}
				release()
				release() // Cleanup paths must not double-decrement.
			} else {
				var rpc *model.RPCError
				if !errors.As(err, &rpc) || rpc.Code != "node_leased" || release != nil {
					t.Fatal("effective lease admitted a stream", release != nil, err)
				}
			}
			n.mu.Lock()
			delete(n.tasks, "busy")
			delete(n.cancels, "busy")
			delete(n.accepting, "busy")
			n.mu.Unlock()
			assertNoLeaseStreams(t, n)
		})
	}
}

func TestLeaseSensitiveStreamPendingPersistence(t *testing.T) {
	for _, outcome := range []string{"committed", "failed", "cancelled"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				n := localNode(t, nil)
				n.leasePending, n.leaseSettled = true, make(chan struct{})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				done := make(chan error, 1)
				go func() {
					release, err := n.reserveLeaseSensitiveStream(ctx)
					if release != nil {
						release()
					}
					done <- err
				}()
				synctest.Wait()
				select {
				case err := <-done:
					t.Fatal("stream passed pending lease persistence", err)
				default:
				}
				if outcome == "cancelled" {
					cancel()
				} else {
					n.mu.Lock()
					if outcome == "committed" {
						n.lease = &model.Lease{ID: "committed", Expires: time.Now().Add(time.Minute)}
					}
					n.leasePending = false
					close(n.leaseSettled)
					n.mu.Unlock()
				}
				err := <-done
				if outcome == "failed" && err != nil || outcome == "committed" && (err == nil || !strings.Contains(err.Error(), "node is leased")) || outcome == "cancelled" && !errors.Is(err, context.Canceled) {
					t.Fatal("incorrect pending-lease outcome", outcome, err)
				}
				assertNoLeaseStreams(t, n)
			})
		})
	}
}

func TestLeaseSensitiveStreamAcquireRace(t *testing.T) {
	n := localNode(t, nil)
	for range 32 {
		start := make(chan struct{})
		var release func()
		var streamErr, leaseErr error
		var lease any
		var wg sync.WaitGroup
		wg.Go(func() { <-start; release, streamErr = n.reserveLeaseSensitiveStream(t.Context()) })
		wg.Go(func() {
			<-start
			lease, leaseErr = n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{}))
		})
		close(start)
		wg.Wait()
		if (streamErr == nil) == (leaseErr == nil) {
			t.Fatal("stream and lease acquisition must have exactly one winner", streamErr, leaseErr)
		}
		if release != nil {
			release()
		}
		if leaseErr == nil {
			if _, err := n.leaseMethod("owner", "leases.release", model.JSON(map[string]string{"id": lease.(model.Lease).ID})); err != nil {
				t.Fatal(err)
			}
		}
		assertNoLeaseStreams(t, n)
	}
}

func TestLeaseSensitiveUploadPublicationAndRecovery(t *testing.T) {
	n := localNode(t, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	q := emptyLeaseUpload()
	conn, done := localLeaseStream(t, n, upload.OpenMethod, q)
	ack := leaseStreamResponse(t, conn, "")
	var ready upload.Ready
	if err := json.Unmarshal(ack.Result, &ready); err != nil {
		t.Fatal(err)
	}
	if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err == nil {
		t.Fatal("lease overlapped an upload waiting for Start")
	}
	if err := upload.WriteRecord(conn, upload.StartRecord, model.JSON(upload.Start{Generation: ready.Generation, Offset: ready.Offset, PrefixSHA256: ready.PrefixSHA256})); err != nil {
		t.Fatal(err)
	}
	if err := upload.WriteRecord(conn, upload.FinishRecord, nil); err != nil {
		t.Fatal(err)
	}
	// Hold the final reply unread after durable publication. Admission must
	// remain held through the reply, not just the incoming payload.
	eventually(t, ctx, func() bool { _, err := os.Stat(filepath.Join(n.Config.WorkDir, q.Path)); return err == nil })
	if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err == nil {
		t.Fatal("lease overlapped upload publication/completion")
	}
	var progress upload.Progress
	if err := readFrame(conn, &progress); err != nil || progress.State != "published" {
		t.Fatal("upload completion", progress, err)
	}
	<-done
	assertNoLeaseStreams(t, n)
	if _, err := n.leaseMethod(n.Identity.ID, "leases.acquire", model.JSON(map[string]any{})); err != nil {
		t.Fatal("completed upload blocked lease", err)
	}
	q.Resume = true
	conn, done = localLeaseStream(t, n, upload.OpenMethod, q)
	leaseStreamResponse(t, conn, "node_leased")
	<-done
	var status upload.Result
	for _, method := range []string{upload.StatusMethod, upload.AbortMethod} {
		if err := n.Call(t.Context(), "", method, map[string]string{"id": q.ID}, &status); err != nil || status.State != "published" {
			t.Fatal("leased upload recovery changed published receipt", method, status, err)
		}
	}
	assertNoLeaseStreams(t, n)
}

func TestLeaseSensitiveUploadPartialRecovery(t *testing.T) {
	n := localNode(t, nil)
	q := emptyLeaseUpload()
	conn, done := localLeaseStream(t, n, upload.OpenMethod, q)
	leaseStreamResponse(t, conn, "")
	_ = conn.Close()
	<-done
	if _, err := n.leaseMethod(n.Identity.ID, "leases.acquire", model.JSON(map[string]any{})); err != nil {
		t.Fatal("inactive partial blocked lease acquisition", err)
	}
	q.Resume = true
	conn, done = localLeaseStream(t, n, upload.OpenMethod, q)
	leaseStreamResponse(t, conn, "node_leased")
	<-done
	var status upload.Result
	if err := n.Call(t.Context(), "", upload.StatusMethod, map[string]string{"id": q.ID}, &status); err != nil || status.State != "staging" || status.Active {
		t.Fatal("leased resume changed partial", status, err)
	}
	if err := n.Call(t.Context(), "", upload.AbortMethod, map[string]string{"id": q.ID}, &status); err != nil || status.State != "aborted" {
		t.Fatal("lease blocked owner cleanup", status, err)
	}
	assertNoLeaseStreams(t, n)
}

func TestLeaseSensitiveStreamFailureCleanup(t *testing.T) {
	for _, method := range []string{upload.OpenMethod, clipboard.OpenMethod, clipboard.PasteMethod} {
		for _, stop := range []string{"invalid", "disconnect", "shutdown"} {
			t.Run(method+"/"+stop, func(t *testing.T) {
				n := localNode(t, nil)
				n.readClipboard = func(context.Context) (clipboard.Value, error) { return clipboard.Value{Text: "fixture"}, nil }
				var request any = clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "files", Files: []clipboard.File{{Name: "output", Size: 1}}}}
				if method == upload.OpenMethod {
					q := emptyLeaseUpload()
					if stop == "invalid" {
						q.Path = "../escape"
					}
					request = q
				} else if stop == "invalid" {
					if method == clipboard.OpenMethod {
						n.readClipboard = func(context.Context) (clipboard.Value, error) {
							return clipboard.Value{}, errors.New("fixture unavailable")
						}
					} else {
						request = clipboard.Request{Protocol: clipboard.Protocol, Content: clipboard.Content{Kind: "invalid"}}
					}
				}
				conn, done := localLeaseStream(t, n, method, request)
				if stop == "invalid" {
					var response model.Response
					if err := readFrame(conn, &response); err != nil || response.Error == "" {
						t.Fatal("invalid operation was accepted", response, err)
					}
				} else {
					leaseStreamResponse(t, conn, "")
					if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err == nil {
						t.Fatal("lease overlapped active stream")
					}
					if stop == "shutdown" {
						if err := n.Close(); err != nil {
							t.Fatal(err)
						}
					} else {
						_ = conn.Close()
					}
				}
				<-done
				assertNoLeaseStreams(t, n)
				if len(n.clipboardSlots) != 0 {
					t.Fatal("clipboard slot leaked")
				}
				if _, err := os.Stat(filepath.Join(n.Config.WorkDir, "output")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("incomplete clipboard file published", err)
				}
			})
		}
	}
}

func TestLeaseSensitiveUploadCapacityCleanup(t *testing.T) {
	n := localNode(t, nil)
	var conns []net.Conn
	var completions []<-chan struct{}
	for _, id := range []string{"first", "second", "limited"} {
		q := emptyLeaseUpload()
		q.ID, q.Path = id, id+".bin"
		conn, done := localLeaseStream(t, n, upload.OpenMethod, q)
		if id == "limited" {
			leaseStreamResponse(t, conn, "upload_limit")
			<-done
			n.mu.Lock()
			active := n.activeInvocations
			n.mu.Unlock()
			if active != 2 {
				t.Fatal("capacity rejection leaked a reservation or released another stream", active)
			}
		} else {
			leaseStreamResponse(t, conn, "")
			conns = append(conns, conn)
			completions = append(completions, done)
		}
	}
	for i, conn := range conns {
		_ = conn.Close()
		<-completions[i]
	}
	assertNoLeaseStreams(t, n)
	if _, err := n.leaseMethod("owner", "leases.acquire", model.JSON(map[string]any{})); err != nil {
		t.Fatal("closed streams or capacity rejection blocked lease", err)
	}
}
