package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/wire"
)

func TestConcurrentLanesShareCarrierAndSurviveSiblingClose(t *testing.T) {
	for _, relay := range []bool{false, true} {
		t.Run(fmt.Sprintf("relay=%v", relay), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			const token = "multi-lane-test-token-1234567890"
			g, err := gateway.New(t.TempDir(), token)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = g.Close() }()
			s := httptest.NewServer(g.Handler())
			defer s.Close()
			makePeer := func(name string) *Peer {
				id, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				p := New(Config{Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: relay}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
				if name == "alpha" {
					// A dependent lane must release setup admission before waiting
					// for its carrier, even when only one setup slot is available.
					p.setups = make(chan struct{}, 1)
				}
				if err := p.Start(ctx); err != nil {
					t.Fatal(err)
				}
				return p
			}
			a, b := makePeer("alpha"), makePeer("beta")
			defer func() { _ = a.Close(); _ = b.Close() }()
			lanes := []Lane{ControlLane, BulkLane, InteractiveLane}
			conns := make([]net.Conn, len(lanes))
			failures := make(chan error, len(lanes))
			var wg sync.WaitGroup
			for i, lane := range lanes {
				wg.Add(1)
				go func() { defer wg.Done(); var err error; conns[i], err = a.OpenLane(ctx, "beta", lane); failures <- err }()
			}
			wg.Wait()
			for range lanes {
				if err := <-failures; err != nil {
					t.Fatal(err)
				}
			}
			for _, conn := range conns {
				defer func() { _ = conn.Close() }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := conn.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				var result [4]byte
				if _, err := io.ReadFull(conn, result[:]); err != nil || string(result[:]) != "ping" {
					t.Fatal(result, err)
				}
			}
			stats := a.SessionStats()
			if len(stats) != 3 {
				t.Fatal("lane sessions duplicated or missing", stats)
			}
			if !relay {
				a.mu.Lock()
				count := len(a.carriers)
				a.mu.Unlock()
				if count != 1 {
					t.Fatal("independent ICE handshakes for traffic lanes", count)
				}
			}
			blockedWrite := make(chan error, 1)
			go func() { _, err := conns[1].Write(make([]byte, 4*1024*1024)); blockedWrite <- err }()
			if _, err := conns[0].Write([]byte("live")); err != nil {
				t.Fatal("bulk backpressure blocked control", err)
			}
			var controlReply [4]byte
			if _, err := io.ReadFull(conns[0], controlReply[:]); err != nil || string(controlReply[:]) != "live" {
				t.Fatal(controlReply, err)
			}
			a.mu.Lock()
			bulk := a.sessions[sessionKey{b.IdentityID(), BulkLane}]
			a.mu.Unlock()
			if err := bulk.mux.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-blockedWrite:
			case <-ctx.Done():
				t.Fatal("bulk write leaked after session close", ctx.Err())
			}
			if _, err := conns[0].Write([]byte("next")); err != nil {
				t.Fatal("bulk closure killed control channel", err)
			}
			var result [4]byte
			if _, err := io.ReadFull(conns[0], result[:]); err != nil || string(result[:]) != "next" {
				t.Fatal(result, err)
			}
			var opened sync.WaitGroup
			for range 16 {
				opened.Add(1)
				go func() {
					defer opened.Done()
					conn, err := a.Open(ctx, b.IdentityID())
					if err == nil {
						_ = conn.Close()
					}
					failures <- err
				}()
			}
			// Drain concurrently because the error queue is deliberately bounded.
			for range 16 {
				if err := <-failures; err != nil {
					t.Fatal(err)
				}
			}
			opened.Wait()
		})
	}
}

func TestSlowPeerSetupDoesNotBlockOtherPeers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const token = "independent-dials-token-12345678"
	const super = "independent-dials-superuser-12345678"
	g, err := gateway.New(t.TempDir(), token, gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	var slowToken atomic.Value
	slowToken.Store("")
	blocked := make(chan struct{})
	var once sync.Once
	handler := g.Handler()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := slowToken.Load().(string)
		if r.Header.Get("Authorization") == "Bearer "+key && len(key) > 0 && r.URL.Path != "/v1/auth" && r.URL.Path != "/v1/connect" {
			once.Do(func() { close(blocked) })
			<-r.Context().Done()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer s.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+"/v1/admin/keys", bytes.NewBufferString(`{"name":"slow"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+super)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var issued struct{ Token string }
	err = json.NewDecoder(resp.Body).Decode(&issued)
	_ = resp.Body.Close()
	if err != nil || issued.Token == "" {
		t.Fatal(issued, err)
	}
	slowToken.Store(issued.Token)
	makePeer := func(name, key string) *Peer {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: s.URL, Token: key, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: true}, func(_ string, conn net.Conn) { _, _ = io.Copy(conn, conn) })
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}
	a, slow, fast := makePeer("caller", token), makePeer("slow", issued.Token), makePeer("fast", token)
	defer func() { _ = a.Close(); _ = slow.Close(); _ = fast.Close() }()
	slowCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		conn, err := a.Open(slowCtx, "slow")
		if conn != nil {
			_ = conn.Close()
		}
		done <- err
	}()
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	fastCtx, stopFast := context.WithTimeout(ctx, time.Second)
	defer stopFast()
	conn, err := a.Open(fastCtx, "fast")
	if err != nil {
		t.Fatal("slow dial blocked unrelated peer", err)
	}
	_ = conn.Close()
	stop()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled setup succeeded")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestStreamLimitWaitRespectsCancellation(t *testing.T) {
	p := New(Config{}, nil)
	p.ctx = context.Background()
	s := &peerSession{lane: ControlLane, outgoing: make(chan struct{}, 128)}
	for range cap(s.outgoing) {
		s.outgoing <- struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := p.openStream(ctx, s); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	for range cap(s.outgoing) {
		<-s.outgoing
	}
	global := p.outgoingSlots[ControlLane]
	for range cap(global) {
		global <- struct{}{}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel2()
	if _, err := p.openStream(ctx2, s); err != context.DeadlineExceeded {
		t.Fatal(err)
	}
	if len(s.outgoing) != 0 {
		t.Fatal("cancelled waiter leaked admission slot")
	}
}

func TestLegacyRawTunnelsAndPolledLogsRemainCompatible(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const token = "legacy-streaming-test-token-12345"
	g, err := gateway.New(t.TempDir(), token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	makePeer := func(name string, handler func(string, net.Conn)) *Peer {
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		p := New(Config{Gateway: s.URL, Token: token, Identity: id, Node: model.Node{ID: id.ID, Name: name, PublicKey: id.Public}, RelayOnly: true}, handler)
		if err := p.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return p
	}
	old := makePeer("old", func(_ string, conn net.Conn) {
		var request model.Request
		if wire.ReadFrame(conn, &request) != nil {
			return
		}
		if request.Method == "tcp.open" {
			_ = wire.WriteFrame(conn, model.Response{Result: json.RawMessage(`{"connected":true}`)})
			_, _ = io.Copy(conn, conn)
			return
		}
		if request.Method == "tasks.logs" {
			var q struct {
				Offset int64 `json:"offset"`
			}
			_ = json.Unmarshal(request.Params, &q)
			text := "tail"
			if q.Offset != 0 {
				text = ""
			}
			_ = wire.WriteFrame(conn, model.Response{Result: model.JSON(map[string]any{"text": text, "offset": 4, "terminal": true})})
		}
	})
	caller := makePeer("caller", nil)
	defer func() { _ = caller.Close(); _ = old.Close() }()
	conn, err := caller.OpenTCP(ctx, "old", "ignored:1234")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err := CloseWrite(conn); err != ErrHalfCloseUnsupported {
		t.Fatal("legacy tunnel claimed half-close", err)
	}
	if _, err := conn.Write([]byte("raw")); err != nil {
		t.Fatal(err)
	}
	var echo [3]byte
	if _, err := io.ReadFull(conn, echo[:]); err != nil || string(echo[:]) != "raw" {
		t.Fatal(echo, err)
	}
	var logs bytes.Buffer
	if err := caller.FollowTaskLogs(ctx, "old", "task", 0, func(chunk model.TaskLogChunk) error { _, err := logs.Write(chunk.Data); return err }); err != nil || logs.String() != "tail" {
		t.Fatal(logs.String(), err)
	}
}
