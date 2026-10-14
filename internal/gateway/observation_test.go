package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"google.golang.org/protobuf/proto"
)

func TestOfflineNotificationFollowsObservationPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	g, err := New(t.TempDir(), commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s := httptest.NewServer(g.Handler())
	defer s.Close()
	observerID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	workerID, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	observer := rawPeer(t, s.URL, commonKey, "observer", legacyUser, observerID)
	defer func() { _ = observer.CloseNow() }()
	worker := rawPeer(t, s.URL, commonKey, "worker", legacyUser, workerID)
	defer func() { _ = worker.CloseNow() }()

	// Occupy the sole database connection so disconnect persistence must wait.
	// Directory reads still return the previous committed online record.
	dbConn, err := g.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dbConn.Close() }()
	waits := g.db.Stats().WaitCount
	notified := make(chan error, 1)
	go func() {
		_, data, err := observer.Read(ctx)
		if err == nil {
			var packet protocol.Packet
			err = proto.Unmarshal(data, &packet)
			if err == nil && (packet.Kind != "offline" || packet.From != workerID.ID) {
				err = fmt.Errorf("unexpected notification: %v", &packet)
			}
		}
		notified <- err
	}()
	_ = worker.CloseNow()
	for g.db.Stats().WaitCount == waits {
		select {
		case <-ctx.Done():
			t.Fatal("disconnect did not reach persistence", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case err := <-notified:
		t.Fatal("notification preceded offline publication", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := dbConn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-notified:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("offline notification not delivered", ctx.Err())
	}
	for _, n := range g.directory(legacyUser) {
		if n.ID == workerID.ID && n.Online {
			t.Fatal("notified identity still online in directory")
		}
	}
}

func TestObservationReadsDoNotWaitForMutationLock(t *testing.T) {
	g, err := New(t.TempDir(), commonKey)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	g.mu.Lock()
	g.owners["worker"] = legacyUser
	g.nodes["worker"] = model.Node{ID: "worker", Name: "committed", UserID: legacyUser}
	if err := g.commit(change{identities: []string{"worker"}, nodes: []string{"worker"}}); err != nil {
		g.mu.Unlock()
		t.Fatal(err)
	}
	// Readers must see the committed name, never this unpublished mutation.
	n := g.nodes["worker"]
	n.Name = "uncommitted"
	g.nodes[n.ID] = n
	for _, path := range []string{"/v1/nodes", "/v1/status"} {
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Header.Set("Authorization", "Bearer "+commonKey)
			w := httptest.NewRecorder()
			g.Handler().ServeHTTP(w, req)
			done <- w
		}()
		select {
		case w := <-done:
			if w.Code != http.StatusOK {
				g.mu.Unlock()
				t.Fatalf("%s returned %d", path, w.Code)
			}
		case <-time.After(time.Second):
			g.mu.Unlock()
			t.Fatalf("%s waited behind mutation lock", path)
		}
	}
	if got := g.observations(legacyUser); len(got) != 1 || got[0].node.Name != "committed" {
		g.mu.Unlock()
		t.Fatalf("uncommitted directory was published: %+v", got)
	}
	// A failed SQLite transaction must leave the read view unchanged.
	g.owners["other"] = legacyUser
	g.nodes["other"] = model.Node{ID: "other", Name: n.Name, UserID: legacyUser}
	if err := g.commit(change{identities: []string{"other"}, nodes: []string{"worker", "other"}}); err == nil {
		g.mu.Unlock()
		t.Fatal("duplicate name committed")
	}
	g.mu.Unlock()
	if got := g.observations(legacyUser); len(got) != 1 || got[0].node.Name != "committed" {
		t.Fatalf("failed mutation was published: %+v", got)
	}
	if got := g.observations("foreign"); len(got) != 0 {
		t.Fatal("observation crossed fleet boundary")
	}
}

func TestRelayQueuePreservesCloseAndOfflineOrder(t *testing.T) {
	c := &connection{out: make(chan *protocol.Packet, 4), outData: make(chan *protocol.Packet, 4)}
	for _, kind := range []string{"data", "close", "offline", "gateway.ping"} {
		if !c.enqueue(&protocol.Packet{Kind: kind, Data: []byte("payload")}) {
			t.Fatal("enqueue failed", kind)
		}
	}
	if p := <-c.out; p.Kind != "gateway.ping" {
		t.Fatal("heartbeat not prioritized")
	}
	for _, kind := range []string{"data", "close", "offline"} {
		if p := <-c.outData; p.Kind != kind {
			t.Fatalf("got %s, want %s", p.Kind, kind)
		}
	}
	if c.queued.Load() != int64(len("payload")) {
		t.Fatal("non-data packets counted as payload")
	}
}

func TestRelayQueueConcurrentByteBound(t *testing.T) {
	c := &connection{outData: make(chan *protocol.Packet, relayQueuePackets)}
	var wg sync.WaitGroup
	for range 256 {
		wg.Go(func() { _ = c.enqueue(&protocol.Packet{Kind: "data", Data: make([]byte, 64<<10)}) })
	}
	wg.Wait()
	if got := c.queued.Load(); got != relayQueueBytes || got != int64(len(c.outData))*(64<<10) {
		t.Fatalf("payload bound/accounting = %d, packets = %d", got, len(c.outData))
	}
	c = &connection{outData: make(chan *protocol.Packet, 1)}
	if !c.enqueue(&protocol.Packet{Kind: "data", Data: []byte("a")}) || c.enqueue(&protocol.Packet{Kind: "data", Data: []byte("b")}) || c.queued.Load() != 1 {
		t.Fatal("full packet queue leaked its byte reservation")
	}
}
