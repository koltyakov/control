package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

func TestConnectionTestStream(t *testing.T) {
	for _, size := range []int64{1, 65537, 9 << 20} {
		q := model.ConnectionTestOptions{Bytes: size, Samples: 3}
		a, b := net.Pipe()
		done := make(chan error, 1)
		go func() { defer func() { _ = b.Close() }(); done <- ServeConnectionTest(b, q) }()
		_ = a.SetDeadline(time.Now().Add(10 * time.Second))
		var ack model.Response
		if err := wire.ReadFrame(a, &ack); err != nil {
			t.Fatal(err)
		}
		latency, upload, download, err := runConnectionTest(a, q)
		_ = a.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		// A local echo can finish within one clock tick, particularly on Windows.
		if latency.Samples != 3 || latency.MinMillis < 0 || latency.MeanMillis < latency.MinMillis || latency.MaxMillis < latency.MeanMillis || upload.Bytes != size || download.Bytes != size || upload.Mbps <= 0 || download.Mbps <= 0 {
			t.Fatalf("invalid measurements: %+v %+v %+v", latency, upload, download)
		}
	}
}

func TestConnectionTestZeroElapsedThroughput(t *testing.T) {
	result := testThroughput(1, 0)
	if result.Bytes != 1 || result.Seconds <= 0 || result.Mbps <= 0 || math.IsInf(result.Mbps, 0) || math.IsNaN(result.Mbps) {
		t.Fatalf("invalid sub-tick throughput: %+v", result)
	}
}

func TestConnectionTestIntegrity(t *testing.T) {
	var payload bytes.Buffer
	if err := sendTestBytes(&payload, 65537); err != nil {
		t.Fatal(err)
	}
	original := append([]byte(nil), payload.Bytes()...)
	if err := receiveTestBytes(bytes.NewReader(original), 65537); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[4] ^= 1
	if err := receiveTestBytes(bytes.NewReader(corrupt), 65537); err == nil {
		t.Fatal("accepted corrupt payload")
	}
	if err := receiveTestBytes(bytes.NewReader(original[:100]), 65537); err != io.ErrUnexpectedEOF {
		t.Fatal("accepted short payload", err)
	}
	if err := receiveTestBytes(bytes.NewReader(original[:len(original)-1]), 65537); err == nil {
		t.Fatal("accepted short checksum")
	}
	if err := sendTestBytes(shortTestWriter{}, 10); err != io.ErrShortWrite {
		t.Fatal("accepted short write", err)
	}
}

type shortTestWriter struct{}

func (shortTestWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestConnectionTestLimits(t *testing.T) {
	for _, q := range []model.ConnectionTestOptions{{Bytes: -1}, {Bytes: 257 << 20}, {Samples: -1}, {Samples: 101}} {
		if _, err := q.Normalize(); err == nil {
			t.Fatal("accepted invalid options", q)
		}
	}
	q, err := (model.ConnectionTestOptions{}).Normalize()
	if err != nil || q.Bytes != 16<<20 || q.Samples != 10 {
		t.Fatal("wrong defaults", q, err)
	}
}

func TestConnectionTestCancellationAfterAcknowledgement(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			const token = "connection-test-cancellation-12345678"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			server := httptest.NewServer(g.Handler())
			defer server.Close()
			entered, exited := make(chan struct{}), make(chan struct{})
			makePeer := func(name string, handler func(string, net.Conn)) *Peer {
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				p := New(Config{Gateway: server.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: relay}, handler)
				if err := p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				return p
			}
			a := makePeer("alpha", nil)
			_ = makePeer("beta", func(_ string, conn net.Conn) {
				defer close(exited)
				var request model.Request
				if wire.ReadFrame(conn, &request) != nil {
					return
				}
				if wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]string{"protocol": model.ConnectionTestProtocol})}) != nil {
					return
				}
				var ping [8]byte
				if _, err := io.ReadFull(conn, ping[:]); err != nil {
					return
				}
				close(entered)
				// Deliberately never echo. Cancellation must release both endpoints.
				_, _ = io.Copy(io.Discard, conn)
			})
			testCtx, stop := context.WithCancel(ctx)
			defer stop()
			done := make(chan error, 1)
			go func() {
				_, err := a.TestConnection(testCtx, "beta", model.ConnectionTestOptions{Bytes: 1024, Samples: 1})
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("test did not reach echo phase", ctx.Err())
			}
			stop()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("wrong cancellation error", err)
				}
			case <-ctx.Done():
				t.Fatal("initiator ignored cancellation")
			}
			select {
			case <-exited:
			case <-ctx.Done():
				t.Fatal("receiver stream remained open")
			}
		})
	}
}
