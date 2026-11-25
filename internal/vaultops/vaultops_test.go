package vaultops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koltyakov/control/internal/client"
	"github.com/koltyakov/control/internal/gateway"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/node"
)

func openTest(t *testing.T, cfg client.StandaloneConfig) (*Runtime, string) {
	t.Helper()
	tunnels := private(t)
	r, err := Open(context.Background(), cfg, tunnels)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, tunnels
}

// private returns a directory that Open may create with owner-only access.
// The testing package creates its own directories with mode 0777&^umask.
func private(t *testing.T) string {
	return filepath.Join(t.TempDir(), "private")
}

func outcome(err error) string {
	var rpc *model.RPCError
	if errors.As(err, &rpc) {
		return rpc.Outcome
	}
	return ""
}

func TestOpenRejectsUnsafeConfiguration(t *testing.T) {
	state := private(t)
	good := client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: state}
	cases := map[string]func(*client.StandaloneConfig, *string){
		"plain remote gateway": func(c *client.StandaloneConfig, _ *string) { c.Gateway.URL = "http://gateway.example" },
		"missing key":          func(c *client.StandaloneConfig, _ *string) { c.Gateway.Key = "" },
		"relative state":       func(c *client.StandaloneConfig, _ *string) { c.StateDir = "state" },
		"relative tunnels":     func(_ *client.StandaloneConfig, dir *string) { *dir = "tunnels" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			cfg, dir := good, private(t)
			change(&cfg, &dir)
			if r, err := Open(context.Background(), cfg, dir); err == nil {
				_ = r.Close()
				t.Fatal("unsafe configuration accepted")
			}
		})
	}
	if runtime.GOOS != "windows" {
		shared := t.TempDir()
		if err := os.Chmod(shared, 0755); err != nil {
			t.Fatal(err)
		}
		if r, err := Open(context.Background(), good, shared); err == nil {
			_ = r.Close()
			t.Fatal("group/other-readable tunnel directory accepted")
		}
	}
	r, err := Open(context.Background(), good, filepath.Join(t.TempDir(), "tunnels"))
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
}

func TestTunnelDirectoryHasOneOwner(t *testing.T) {
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: private(t)}
	r, dir := openTest(t, cfg)
	if second, err := Open(context.Background(), cfg, dir); err == nil {
		_ = second.Close()
		t.Fatal("two runtimes owned one tunnel directory")
	}
	_ = r.Close()
	again, err := Open(context.Background(), cfg, dir)
	if err != nil {
		t.Fatal("closed runtime kept directory ownership", err)
	}
	_ = again.Close()
}

func TestRejectsUnsafeOperationsBeforeSending(t *testing.T) {
	// The gateway address refuses connections: anything that reached the
	// network would fail with an outcome other than not_sent.
	r, _ := openTest(t, client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: private(t)})
	h := r.NewSession(context.Background())
	defer func() { _ = h.Close() }()
	cases := []struct{ op, args string }{
		{"files.read", `{}`},
		{OpCall, `{"target":"w","method":"tcp.open","params":{"address":"127.0.0.1:22"}}`},
		{OpCall, `{"target":"w","method":"artifacts.open"}`},
		{OpCall, `{"target":"w","method":"files.upload.open"}`},
		{OpCall, `{"target":"w","method":"access.grant"}`},
		{OpCall, `{"target":"w","method":"secrets.set"}`},
		{OpCall, `{"target":"w","method":"update.install"}`},
		{OpCall, `{"target":"w","method":"clipboard.paste"}`},
		{OpCall, `{"target":"w","method":"shell.open"}`},
		{OpCall, `{"target":"w","method":"connection.open"}`},
		{OpCall, `{"target":"w","method":"exec.run","params":[1]}`},
		{OpCall, `{"target":"w","method":"bad method"}`},
		{OpCall, `{"target":"w"}`},
		{OpCall, `{"target":"w","method":"node.describe","gateway":"https://elsewhere"}`},
		{OpCall, `{"method":"node.describe"} {}`},
		{OpCall, `[]`},
		{OpMachines, `{"node":""}`},
		{OpSession, `{"stateDir":"/tmp"}`},
		{OpForwardStart, `{"node":"w","address":"127.0.0.1:80","route":"proxy"}`},
		{OpForwardStop, `{}`},
		{OpTunnelStart, `{"id":"t","node":"w","address":"127.0.0.1:80","listen":"127.0.0.1:9000","socks":true}`},
		{OpTunnelStart, `{"id":"../t","node":"w","address":"127.0.0.1:80","listen":"127.0.0.1:9000"}`},
		{OpTunnelDispose, `{"id":"a/b"}`},
	}
	for _, c := range cases {
		if _, err := h.Do(context.Background(), c.op, json.RawMessage(c.args)); err == nil || outcome(err) != "not_sent" {
			t.Errorf("%s %s: expected not_sent rejection, got %v", c.op, c.args, err)
		}
	}
	// The broker keeps the client's forward validation and adds no listener
	// or destination policy: these reach the gateway, which refuses them.
	for _, args := range []string{
		`{"node":"w","socks":true,"allowNonLoopback":true,"listen":"0.0.0.0:0"}`,
		`{"node":"w","address":"10.0.0.1:80","listen":"0.0.0.0:0","reverse":true}`,
	} {
		if _, err := h.Do(context.Background(), OpForwardStart, json.RawMessage(args)); err == nil || outcome(err) == "not_sent" {
			t.Errorf("forward %s was rejected by broker policy: %v", args, err)
		}
	}
	if result, err := h.Do(context.Background(), OpForwardList, nil); err != nil || string(result) != "[]" {
		t.Fatal("empty forward list", string(result), err)
	}
	if result, err := h.Do(context.Background(), OpTunnelList, json.RawMessage(`{}`)); err != nil || string(result) != "[]" {
		t.Fatal("empty tunnel list", string(result), err)
	}
	if _, err := h.Do(context.Background(), OpTunnelDispose, json.RawMessage(`{"id":"absent"}`)); err != nil {
		t.Fatal("dispose of an absent tunnel is not idempotent", err)
	}
	_ = h.Close()
	if _, err := h.Do(context.Background(), OpForwardList, nil); outcome(err) != "not_sent" {
		t.Fatal("closed session accepted an operation", err)
	}
}

func TestSessionsEndWithConnectionAndRuntime(t *testing.T) {
	r, _ := openTest(t, client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: private(t)})
	connection, disconnect := context.WithCancel(context.Background())
	h := r.NewSession(connection)
	disconnect()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session outlived its connection")
	}
	r.mu.Lock()
	remaining := len(r.sessions)
	r.mu.Unlock()
	if remaining != 0 {
		t.Fatal("ended session still registered")
	}
	var admitted []*Handler
	for range MaxSessions {
		admitted = append(admitted, r.NewSession(context.Background()))
	}
	over := r.NewSession(context.Background())
	if _, err := over.Do(context.Background(), OpTunnelList, nil); outcome(err) != "not_sent" || !strings.Contains(err.Error(), "limit") {
		t.Fatal("session beyond the limit was admitted", err)
	}
	if err := over.Close(); err != nil {
		t.Fatal(err)
	}
	_ = admitted[0].Close()
	if _, err := r.NewSession(context.Background()).Do(context.Background(), OpTunnelList, nil); err != nil {
		t.Fatal("released capacity was not reused", err)
	}
	other := admitted[1]
	_ = r.Close()
	select {
	case <-other.done:
	case <-time.After(5 * time.Second):
		t.Fatal("session outlived the runtime")
	}
	if _, err := r.NewSession(context.Background()).Do(context.Background(), OpTunnelList, nil); outcome(err) != "not_sent" {
		t.Fatal("closed runtime admitted a session", err)
	}
}

func retainedEntry(id, listen string) client.PersistentTunnelInfo {
	return client.PersistentTunnelInfo{
		PersistentTunnelSpec: client.PersistentTunnelSpec{ID: id, ForwardSpec: client.ForwardSpec{Node: "worker", Address: "127.0.0.1:80", Listen: listen}},
		NodeID:               strings.Repeat("0123456789abcdef", 4),
		Created:              time.Date(2026, 11, 25, 9, 0, 0, 123456789, time.UTC),
	}
}

func TestValidateRetainedTunnelState(t *testing.T) {
	withTTL := func(info client.PersistentTunnelInfo, seconds int64, expires time.Time) client.PersistentTunnelInfo {
		info.TTLSeconds, info.Expires = seconds, expires
		return info
	}
	base := retainedEntry("a", "127.0.0.1:9000")
	other := retainedEntry("b", "127.0.0.1:9001")
	reverse := func(id, node string) client.PersistentTunnelInfo {
		info := retainedEntry(id, "0.0.0.0:9000")
		info.Reverse, info.NodeID = true, node
		return info
	}
	full := make([]client.PersistentTunnelInfo, client.MaxRetainedTunnels+1)
	for i := range full {
		full[i] = retainedEntry(fmt.Sprintf("t%d", i), fmt.Sprintf("127.0.0.1:%d", 9000+i))
	}
	valid := map[string][]client.PersistentTunnelInfo{
		"empty":             nil,
		"no TTL":            {base},
		"exact TTL expiry":  {withTTL(base, 60, base.Created.Add(time.Minute)), other},
		"maximum entries":   full[:client.MaxRetainedTunnels],
		"reverse per node":  {reverse("r1", strings.Repeat("a", 64)), reverse("r2", strings.Repeat("b", 64)), base},
		"forward + reverse": {base, func() client.PersistentTunnelInfo { info := base; info.ID, info.Reverse = "r", true; return info }()},
	}
	for name, infos := range valid {
		if err := client.ValidateRetainedTunnelState(infos); err != nil {
			t.Errorf("%s rejected: %v", name, err)
		}
	}
	mutate := func(change func(*client.PersistentTunnelInfo)) []client.PersistentTunnelInfo {
		info := base
		change(&info)
		return []client.PersistentTunnelInfo{info}
	}
	invalid := map[string][]client.PersistentTunnelInfo{
		"too many entries":     full,
		"duplicate ID":         {base, func() client.PersistentTunnelInfo { info := other; info.ID = "a"; return info }()},
		"duplicate listener":   {base, func() client.PersistentTunnelInfo { info := other; info.Listen = base.Listen; return info }()},
		"duplicate reverse":    {reverse("r1", base.NodeID), reverse("r2", base.NodeID)},
		"uppercase node ID":    mutate(func(i *client.PersistentTunnelInfo) { i.NodeID = strings.ToUpper(i.NodeID) }),
		"short node ID":        mutate(func(i *client.PersistentTunnelInfo) { i.NodeID = i.NodeID[1:] }),
		"non-hex node ID":      mutate(func(i *client.PersistentTunnelInfo) { i.NodeID = "g" + i.NodeID[1:] }),
		"missing node ID":      mutate(func(i *client.PersistentTunnelInfo) { i.NodeID = "" }),
		"missing created":      mutate(func(i *client.PersistentTunnelInfo) { i.Created = time.Time{} }),
		"expiry without TTL":   mutate(func(i *client.PersistentTunnelInfo) { i.Expires = i.Created.Add(time.Hour) }),
		"TTL without expiry":   mutate(func(i *client.PersistentTunnelInfo) { i.TTLSeconds = 60 }),
		"TTL expiry drift":     {withTTL(base, 60, base.Created.Add(time.Minute+time.Nanosecond))},
		"TTL expiry restarted": {withTTL(base, 60, base.Created.Add(2*time.Minute))},
		"negative TTL":         {withTTL(base, -1, time.Time{})},
		"invalid ID":           mutate(func(i *client.PersistentTunnelInfo) { i.ID = "../a" }),
		"proxy mode":           mutate(func(i *client.PersistentTunnelInfo) { i.SOCKS = true }),
		"ephemeral listener":   mutate(func(i *client.PersistentTunnelInfo) { i.Listen = "127.0.0.1:0" }),
		"missing target name":  mutate(func(i *client.PersistentTunnelInfo) { i.Node = "" }),
	}
	for name, infos := range invalid {
		if err := client.ValidateRetainedTunnelState(infos); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestOpenRejectsInvalidRetainedState(t *testing.T) {
	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: "http://127.0.0.1:1", Key: "key"}, StateDir: private(t)}
	r, root := openTest(t, cfg)
	_ = r.Close()
	dirs, err := filepath.Glob(filepath.Join(root, "*"))
	if err != nil || len(dirs) != 1 {
		t.Fatal("tunnel scope directory", dirs, err)
	}
	path := filepath.Join(dirs[0], "tunnels.json")
	good := retainedEntry("good", "127.0.0.1:9000")
	drifted := retainedEntry("drifted", "127.0.0.1:9001")
	drifted.TTLSeconds, drifted.Expires = 60, drifted.Created.Add(90*time.Second)
	for name, infos := range map[string][]client.PersistentTunnelInfo{
		"drifted expiry": {good, drifted},
		"uppercase node": {func() client.PersistentTunnelInfo {
			info := good
			info.NodeID = strings.ToUpper(info.NodeID)
			return info
		}()},
	} {
		if err := os.WriteFile(path, model.JSON(infos), 0600); err != nil {
			t.Fatal(err)
		}
		if r, err := Open(context.Background(), cfg, root); err == nil {
			_ = r.Close()
			t.Fatalf("%s: invalid retained state restored", name)
		}
	}
	// A rejected restore releases ownership; valid state opens again.
	if err := os.WriteFile(path, model.JSON([]client.PersistentTunnelInfo{good}), 0600); err != nil {
		t.Fatal(err)
	}
	r, err = Open(context.Background(), cfg, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	listed := do[[]client.PersistentTunnelInfo](t, r.NewSession(context.Background()), OpTunnelList, nil)
	if len(listed) != 1 || listed[0].ID != "good" || !listed[0].Created.Equal(good.Created) {
		t.Fatal("valid state was not restored offline", listed)
	}
}

var fixedPort atomic.Uint32

// fixedAddress picks a loopback port below the common ephemeral ranges, so a
// retained listener can rebind it after a runtime restart.
func fixedAddress(t *testing.T) string {
	t.Helper()
	for range 2700 {
		port := 30000 + (fixedPort.Add(1)-1)%2700
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			continue
		}
		address := listener.Addr().String()
		_ = listener.Close()
		return address
	}
	t.Fatal("no fixed test port available")
	return ""
}

func echoServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer func() { _ = conn.Close() }(); _, _ = io.Copy(conn, conn) }()
		}
	}()
	return listener.Addr().String()
}

func echo(address string) error {
	conn, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	var reply [4]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return err
	}
	if string(reply[:]) != "ping" {
		return fmt.Errorf("unexpected echo %q", reply)
	}
	return nil
}

func do[T any](t *testing.T, h *Handler, op string, args any) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	raw, err := h.Do(ctx, op, model.JSON(args))
	if err != nil {
		t.Fatalf("%s: %v", op, err)
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("%s result %s: %v", op, raw, err)
	}
	return value
}

func waitTunnels(t *testing.T, h *Handler, count int) []client.PersistentTunnelInfo {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		infos := do[[]client.PersistentTunnelInfo](t, h, OpTunnelList, map[string]any{})
		ready := len(infos) == count
		for _, info := range infos {
			ready = ready && info.State == "active"
		}
		if ready {
			return infos
		}
		if time.Now().After(deadline) {
			t.Fatalf("tunnels did not become ready: %+v", infos)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFleetOperationsAndRetainedTunnels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const super = "vaultops-superuser-key-1234567890"
	g, err := gateway.New(t.TempDir(), "", gateway.Options{SuperuserKey: super})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g.Handler())
	var closeOnce sync.Once
	closeGateway := func() { closeOnce.Do(func() { server.Close(); _ = g.Close() }) }
	defer closeGateway()
	// The broker and its worker use a real fleet account, not the operator key.
	var account struct {
		Token string `json:"token"`
	}
	if err := (client.Admin{URL: server.URL, Key: super}).JSON(ctx, http.MethodPost, "/v1/admin/users", map[string]string{"name": "vault"}, &account); err != nil || account.Token == "" {
		t.Fatal("account creation", err)
	}
	key := account.Token
	worker, err := node.New(node.Config{Name: "worker", Gateway: server.URL, Token: key, DataDir: t.TempDir(), WorkDir: t.TempDir(), RelayOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = worker.Close() }()
	if err = worker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// A project runbook store in the working directory must stay invisible.
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".control", "runbooks"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".control", "runbooks", "local.md"), []byte("# Broker host secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(project)

	cfg := client.StandaloneConfig{Gateway: client.Admin{URL: server.URL, Key: key}, StateDir: private(t), RelayOnly: true}
	tunnelRoot := private(t)
	r, err := Open(ctx, cfg, tunnelRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	h := r.NewSession(ctx)

	// Superuser credentials hold administrative authority and are refused for
	// fleet operations; the local tunnel table stays readable.
	superCfg := client.StandaloneConfig{Gateway: client.Admin{URL: server.URL, Key: super}, StateDir: private(t), RelayOnly: true}
	superRuntime, err := Open(ctx, superCfg, private(t))
	if err != nil {
		t.Fatal(err)
	}
	superSession := superRuntime.NewSession(ctx)
	for _, op := range []string{OpMachines, OpSession} {
		if _, err := superSession.Do(ctx, op, nil); err == nil || !strings.Contains(err.Error(), "superuser") {
			t.Fatal("superuser broker accepted", op, err)
		}
	}
	if _, err := superSession.Do(ctx, OpTunnelStart, model.JSON(client.PersistentTunnelSpec{ID: "t", ForwardSpec: client.ForwardSpec{Node: "worker", Address: "127.0.0.1:1", Listen: fixedAddress(t)}})); err == nil {
		t.Fatal("superuser broker started a retained tunnel")
	}
	if listed := do[[]client.PersistentTunnelInfo](t, superSession, OpTunnelList, nil); len(listed) != 0 {
		t.Fatal("superuser tunnel list", listed)
	}
	_ = superRuntime.Close()

	machines := do[[]map[string]any](t, h, OpMachines, map[string]any{"compact": true, "node": "worker"})
	if len(machines) != 1 || machines[0]["name"] != "worker" || machines[0]["capabilities"] != nil {
		t.Fatal("compact machine projection", machines)
	}
	session := do[client.SessionInfo](t, h, OpSession, nil)
	if session.Role != "client" || session.OwnerID == "" || session.TransportID == "" {
		t.Fatal("session identity", session)
	}
	description := do[map[string]json.RawMessage](t, h, OpCall, CallArgs{Target: "worker", Method: "node.describe"})
	var catalog struct {
		Sources []struct {
			Name string `json:"name"`
		} `json:"sources"`
		Runbooks []struct {
			Title string `json:"title"`
		} `json:"runbooks"`
	}
	if err := json.Unmarshal(description["runbooks"], &catalog); err != nil {
		t.Fatal(err)
	}
	for _, source := range catalog.Sources {
		if source.Name == "project" || source.Name == "global" {
			t.Fatal("node.describe exposed broker-local runbook stores", string(description["runbooks"]))
		}
	}
	if strings.Contains(string(description["runbooks"]), "Broker host secret") {
		t.Fatal("broker project runbook leaked")
	}
	pool := do[model.PoolActivitySnapshot](t, h, OpCall, CallArgs{Method: "activities.pool"})
	if len(pool.Nodes) != 1 {
		t.Fatal("pool activity", pool)
	}
	if _, err := h.Do(ctx, OpCall, model.JSON(CallArgs{Method: "files.list"})); err == nil {
		t.Fatal("untargeted worker method reached the broker host")
	}

	// A forward outlives the request context that started it.
	target := echoServer(t)
	startCtx, startCancel := context.WithCancel(ctx)
	raw, err := h.Do(startCtx, OpForwardStart, model.JSON(client.ForwardSpec{Node: "worker", Address: target}))
	startCancel()
	if err != nil {
		t.Fatal(err)
	}
	var forward client.ForwardInfo
	if err := json.Unmarshal(raw, &forward); err != nil {
		t.Fatal(err)
	}
	if host, _, _ := net.SplitHostPort(forward.Listen); host != "127.0.0.1" {
		t.Fatal("default forward listener is not loopback", forward)
	}
	if err := echo(forward.Listen); err != nil {
		t.Fatal("forward after request cancellation", err)
	}
	other := r.NewSession(ctx)
	if forwards := do[[]client.ForwardInfo](t, other, OpForwardList, nil); len(forwards) != 0 {
		t.Fatal("sessions share forwards", forwards)
	}
	if _, err := other.Do(ctx, OpForwardStop, model.JSON(IDArgs{ID: forward.ID})); err == nil {
		t.Fatal("another session stopped a forward")
	}
	_ = other.Close()

	// A cancelled grant context abandons the request.
	cancelled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	if _, err := h.Do(cancelled, OpCall, model.JSON(CallArgs{Target: "worker", Method: "tasks.get", Params: json.RawMessage(`{"id":"absent","waitSeconds":30}`)})); err == nil {
		t.Fatal("cancelled request succeeded")
	}

	spec := client.PersistentTunnelSpec{ID: "echo", ForwardSpec: client.ForwardSpec{Node: "worker", Address: target, Listen: fixedAddress(t)}}
	reverse := client.PersistentTunnelSpec{ID: "reverse", TTLSeconds: 3600, ForwardSpec: client.ForwardSpec{Node: "worker", Address: target, Listen: fixedAddress(t), Reverse: true}}
	// short must outlive one runtime restart, then expire under the second.
	short := client.PersistentTunnelSpec{ID: "short", TTLSeconds: 8, ForwardSpec: client.ForwardSpec{Node: "worker", Address: target, Listen: fixedAddress(t)}}
	accepted := map[string]client.PersistentTunnelInfo{}
	for _, tunnel := range []client.PersistentTunnelSpec{spec, reverse, short} {
		info := do[client.PersistentTunnelInfo](t, h, OpTunnelStart, tunnel)
		if info.NodeID != worker.Identity.ID || info.Expires.IsZero() != (tunnel.TTLSeconds == 0) {
			t.Fatal("tunnel acceptance", info)
		}
		if tunnel.TTLSeconds > 0 && !info.Expires.Equal(info.Created.Add(time.Duration(tunnel.TTLSeconds)*time.Second)) {
			t.Fatal("tunnel TTL was not an absolute deadline", info)
		}
		accepted[tunnel.ID] = info
	}
	waitTunnels(t, h, 3)
	for _, tunnel := range []client.PersistentTunnelSpec{spec, reverse, short} {
		if err := echo(tunnel.Listen); err != nil {
			t.Fatal("retained tunnel", tunnel.ID, err)
		}
	}

	// Ending the creating session leaves retained tunnels running; its
	// ephemeral forward ends with it.
	_ = h.Close()
	if err := echo(spec.Listen); err != nil {
		t.Fatal("retained tunnel depended on its session", err)
	}
	if err := echo(forward.Listen); err == nil {
		t.Fatal("ephemeral forward outlived its session")
	}

	// Nothing on disk carries the account key or a management endpoint.
	err = filepath.Walk(tunnelRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		if name := info.Name(); name == "service.json" || name == "endpoint.json" {
			return fmt.Errorf("management file %s published", path)
		}
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), key) {
			return fmt.Errorf("%s contains the account key", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := filepath.Glob(filepath.Join(tunnelRoot, "*", "tunnels.json"))
	if err != nil || len(saved) != 1 {
		t.Fatal("retained definitions", saved, err)
	}
	definitions := func() []client.PersistentTunnelInfo {
		var infos []client.PersistentTunnelInfo
		data, err := os.ReadFile(saved[0])
		if err == nil {
			err = json.Unmarshal(data, &infos)
		}
		if err != nil {
			t.Fatal(err)
		}
		return infos
	}

	if err := client.ValidateRetainedTunnelState(definitions()); err != nil {
		t.Fatal("runtime wrote state the strict validator rejects", err)
	}

	// A new runtime restores the original definitions, including the
	// absolute expiry, instead of restarting a TTL.
	_ = r.Close()
	if err := echo(spec.Listen); err == nil {
		t.Fatal("closed runtime kept a retained listener")
	}
	r, err = Open(ctx, cfg, tunnelRoot)
	if err != nil {
		t.Fatal(err)
	}
	h = r.NewSession(ctx)
	restored := waitTunnels(t, h, 3)
	for _, info := range restored {
		original := accepted[info.ID]
		if !info.Created.Equal(original.Created) || !info.Expires.Equal(original.Expires) {
			t.Fatal("restore replaced a definition", info, original)
		}
	}
	if !time.Now().Before(accepted["short"].Expires) {
		t.Fatal("short TTL elapsed before the restart was checked; the test machine is too slow")
	}
	for _, tunnel := range []client.PersistentTunnelSpec{spec, short} {
		if err := echo(tunnel.Listen); err != nil {
			t.Fatal("restored tunnel", tunnel.ID, err)
		}
	}

	// The restored deadline closes and durably removes the short tunnel;
	// tunnels without a TTL, or with a later one, remain.
	deadline := accepted["short"].Expires.Add(10 * time.Second)
	for {
		listed := do[[]client.PersistentTunnelInfo](t, h, OpTunnelList, nil)
		_, dialErr := net.DialTimeout("tcp", short.Listen, 100*time.Millisecond)
		stored := false
		for _, info := range definitions() {
			stored = stored || info.ID == "short"
		}
		if len(listed) == 2 && dialErr != nil && !stored {
			if time.Now().Before(accepted["short"].Expires) {
				t.Fatal("short tunnel removed before its absolute expiry")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("short tunnel did not expire", listed, dialErr, stored)
		}
		time.Sleep(50 * time.Millisecond)
	}
	waitTunnels(t, h, 2)
	if err := echo(spec.Listen); err != nil {
		t.Fatal("tunnel without TTL expired", err)
	}

	// With the gateway unreachable, a restarted broker still restores,
	// lists and disposes its retained tunnels.
	_ = r.Close()
	closeGateway()
	r, err = Open(ctx, cfg, tunnelRoot)
	if err != nil {
		t.Fatal("offline restore", err)
	}
	h = r.NewSession(ctx)
	offline := do[[]client.PersistentTunnelInfo](t, h, OpTunnelList, nil)
	if len(offline) != 2 {
		t.Fatal("offline list", offline)
	}
	for _, info := range offline {
		original := accepted[info.ID]
		if info.State == "active" || !info.Created.Equal(original.Created) || !info.Expires.Equal(original.Expires) {
			t.Fatal("offline restore", info, original)
		}
	}
	for _, id := range []string{"echo", "reverse", "echo"} {
		if result := do[map[string]bool](t, h, OpTunnelDispose, IDArgs{ID: id}); !result["disposed"] {
			t.Fatal("dispose", id, result)
		}
	}
	if remaining := do[[]client.PersistentTunnelInfo](t, h, OpTunnelList, nil); len(remaining) != 0 {
		t.Fatal("offline dispose", remaining)
	}
	_ = r.Close()
	r, err = Open(ctx, cfg, tunnelRoot)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := do[[]client.PersistentTunnelInfo](t, r.NewSession(ctx), OpTunnelList, nil); len(remaining) != 0 || len(definitions()) != 0 {
		t.Fatal("disposal was not durable", remaining)
	}
}
