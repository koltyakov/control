package gateway

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/koltyakov/control/internal/enrollment"
	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/protocol"
	"github.com/koltyakov/control/internal/update"
	"google.golang.org/protobuf/proto"
)

// labelPeer registers a machine whose signed registration reports labels.
// edit may add fields the node asserts in its own signed bytes.
func labelPeer(t *testing.T, serverURL, token, name string, id *identity.Identity, labels map[string]string, edit func(*model.Node)) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, serverURL+"/v1/connect", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}})
	if err != nil {
		t.Fatal(err)
	}
	_, challenge, err := ws.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	n := model.Node{ID: id.ID, PublicKey: id.Public, Name: name, Labels: labels}
	if edit != nil {
		edit(&n)
	}
	hello := Hello{Version: model.Version, NodeBytes: model.JSON(n)}
	hello.Signature = ed25519.Sign(id.Private, hello.Message(challenge))
	if err = ws.Write(ctx, websocket.MessageBinary, model.JSON(hello)); err != nil {
		t.Fatal(err)
	}
	if _, ready, err := ws.Read(ctx); err != nil || string(ready) != "ready" {
		t.Fatal("registration rejected", string(ready), err)
	}
	t.Cleanup(func() { _ = ws.CloseNow() })
	return ws
}

func labelPatch(set map[string]string, remove, reset []string) map[string]model.LabelPatch {
	return map[string]model.LabelPatch{"labels": {Set: set, Remove: remove, Reset: reset}}
}

func directoryNode(t *testing.T, a testAdmin, id string) model.Node {
	t.Helper()
	var nodes []model.Node
	if err := a.JSON(context.Background(), "GET", "/v1/nodes", nil, &nodes); err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatal("machine missing from directory", id)
	return model.Node{}
}

func countAudit(t *testing.T, g *Gateway, action string) int {
	t.Helper()
	var count int
	if err := g.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// The owner overlay changes the effective labels every directory reader sees,
// keeps both origins visible, never rewrites the node's registration, notifies
// connected peers, survives restart and reconnection, and is audited.
func TestOwnerLabelOverlay(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	reported := map[string]string{"zone": "lab", "gpu": "none", "lan": "office"}
	ws := labelPeer(t, s.URL, a.Key, "render", id, reported, func(n *model.Node) {
		// A node cannot claim owner-set labels in its signed registration.
		n.OwnerLabels = map[string]string{"pool": "forged"}
		n.RemovedLabels = []string{"zone"}
		n.NodeLabels = map[string]string{"x": "y"}
	})
	n := directoryNode(t, a, id.ID)
	if !maps.Equal(n.Labels, reported) || n.OwnerLabels != nil || n.RemovedLabels != nil || n.NodeLabels != nil {
		t.Fatal("registration asserted owner label fields", n)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	path := "/v1/fleet/nodes/" + id.ID
	var view model.MachineLabels
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{"pool": "render", "gpu": "a100"}, []string{"zone"}, nil), &view); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"pool": "render", "gpu": "a100", "lan": "office"}
	if !maps.Equal(view.Labels, want) || !maps.Equal(view.NodeLabels, reported) || !maps.Equal(view.OwnerLabels, map[string]string{"pool": "render", "gpu": "a100"}) || !slices.Equal(view.RemovedLabels, []string{"zone"}) {
		t.Fatal("label view", view)
	}
	_, data, err := ws.Read(ctx)
	var packet protocol.Packet
	if err != nil || proto.Unmarshal(data, &packet) != nil || packet.Kind != "directory.changed" || packet.From != update.GatewaySender {
		t.Fatal("missing cache invalidation", &packet, err)
	}
	n = directoryNode(t, a, id.ID)
	if !maps.Equal(n.Labels, want) || !maps.Equal(n.NodeLabels, reported) || !slices.Equal(n.RemovedLabels, []string{"zone"}) || n.OwnerLabels["pool"] != "render" {
		t.Fatal("directory labels", n)
	}
	var status model.PoolActivitySnapshot
	if err := a.JSON(ctx, "GET", "/v1/status", nil, &status); err != nil || len(status.Nodes) != 1 || !maps.Equal(status.Nodes[0].Labels, want) || !maps.Equal(status.Nodes[0].NodeLabels, reported) || status.Nodes[0].OwnerLabels["gpu"] != "a100" {
		t.Fatal("status labels", status.Nodes, err)
	}
	var peer model.Node
	if err := a.JSON(ctx, "GET", "/v1/peers/"+id.ID, nil, &peer); err != nil || !maps.Equal(peer.Labels, want) {
		t.Fatal("peer lookup labels", peer.Labels, err)
	}
	g.mu.Lock()
	stored := g.nodes[id.ID].Labels
	g.mu.Unlock()
	if !maps.Equal(stored, reported) {
		t.Fatal("node registration was rewritten", stored)
	}
	var raw []byte
	var record model.Node
	if err := g.db.QueryRow(`SELECT data FROM nodes WHERE id=?`, id.ID).Scan(&raw); err != nil || json.Unmarshal(raw, &record) != nil || !maps.Equal(record.Labels, reported) || strings.Contains(string(raw), "ownerLabels") || strings.Contains(string(raw), "nodeLabels") {
		t.Fatal("stored registration contains owner labels", string(raw), err)
	}
	if countAudit(t, g, "machine.labels") != 1 {
		t.Fatal("label change was not audited once")
	}
	// Repeating the same change commits nothing.
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{"pool": "render"}, []string{"zone"}, nil), &view); err != nil || countAudit(t, g, "machine.labels") != 1 {
		t.Fatal("no-op label change was committed", err)
	}
	// Reset returns node-reported values; removal hides a key even before the
	// node reports it.
	view = model.MachineLabels{}
	if err := a.JSON(ctx, "PATCH", path, labelPatch(nil, []string{"future"}, []string{"gpu", "zone"}), &view); err != nil {
		t.Fatal(err)
	}
	want = map[string]string{"pool": "render", "gpu": "none", "zone": "lab", "lan": "office"}
	if !maps.Equal(view.Labels, want) || !slices.Equal(view.RemovedLabels, []string{"future"}) {
		t.Fatal("reset", view)
	}

	// Restart and reconnection with the unchanged node configuration keep the
	// overlay; a later node-reported "future" label stays hidden.
	_ = ws.CloseNow()
	_ = g.Close()
	s.Close()
	g, err = New(filepath.Dir(g.path), commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.Close() }()
	s2 := newTestServer(t, g)
	a.URL = s2
	reported["future"] = "yes"
	labelPeer(t, s2, a.Key, "render", id, reported, nil)
	n = directoryNode(t, a, id.ID)
	if !maps.Equal(n.Labels, want) || !maps.Equal(n.NodeLabels, reported) || !n.Online {
		t.Fatal("overlay lost on restart or reconnection", n)
	}
	if n.UserID != alice.ID {
		t.Fatal("fleet changed", n.UserID)
	}
	// Removing every owner entry deletes the overlay row.
	var cleared model.MachineLabels
	if err := a.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(nil, nil, []string{"pool", "future"}), &cleared); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := g.db.QueryRow(`SELECT count(*) FROM machine_labels`).Scan(&rows); err != nil || rows != 0 || len(cleared.OwnerLabels) != 0 || !maps.Equal(cleared.Labels, reported) || !maps.Equal(cleared.NodeLabels, reported) {
		t.Fatal("empty overlay retained", rows, cleared, err)
	}
	n = directoryNode(t, a, id.ID)
	if !maps.Equal(n.Labels, reported) || n.NodeLabels != nil || n.OwnerLabels != nil {
		t.Fatal("directory after full reset", n)
	}
}

func newTestServer(t *testing.T, g *Gateway) string {
	t.Helper()
	s := httptest.NewServer(g.Handler())
	t.Cleanup(s.Close)
	return s.URL
}

func TestOwnerLabelValidation(t *testing.T) {
	_, s := installationFixture(t)
	_, a := createTestUser(t, s, "alice")
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	labelPeer(t, s.URL, a.Key, "render", id, nil, nil)
	ctx := context.Background()
	path := "/v1/fleet/nodes/" + id.ID
	many := map[string]string{}
	for i := range model.MaxOwnerLabels + 1 {
		many["k"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "v"
	}
	invalid := []any{
		map[string]any{},
		map[string]any{"labels": map[string]any{}},
		map[string]any{"labels": map[string]any{"set": map[string]string{"pool": "x"}}, "name": "other"},
		map[string]any{"labels": map[string]any{"set": map[string]string{"pool": "x"}}, "disabled": true},
		map[string]any{"labels": map[string]any{"set": map[string]string{"pool": "x"}}, "force": true},
		labelPatch(map[string]string{"": "x"}, nil, nil),
		labelPatch(map[string]string{"-pool": "x"}, nil, nil),
		labelPatch(map[string]string{"pool-": "x"}, nil, nil),
		labelPatch(map[string]string{"po ol": "x"}, nil, nil),
		labelPatch(map[string]string{"po\nol": "x"}, nil, nil),
		labelPatch(map[string]string{strings.Repeat("k", model.MaxLabelKeyBytes+1): "x"}, nil, nil),
		labelPatch(map[string]string{"pool": ""}, nil, nil),
		labelPatch(map[string]string{"pool": "a b"}, nil, nil),
		labelPatch(map[string]string{"pool": "a,b"}, nil, nil),
		labelPatch(map[string]string{"pool": "a=b"}, nil, nil),
		labelPatch(map[string]string{"pool": "é"}, nil, nil),
		labelPatch(map[string]string{"pool": strings.Repeat("v", model.MaxLabelValueBytes+1)}, nil, nil),
		labelPatch(map[string]string{"pool": "x"}, []string{"pool"}, nil),
		labelPatch(nil, []string{"zone"}, []string{"zone"}),
		labelPatch(nil, []string{"bad key"}, nil),
		labelPatch(many, nil, nil),
	}
	for _, body := range invalid {
		if err := a.JSON(ctx, "PATCH", path, body, nil); err == nil || !strings.Contains(err.Error(), "400") {
			t.Fatalf("invalid label change accepted: %v: %v", body, err)
		}
	}
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{strings.Repeat("k", model.MaxLabelKeyBytes): strings.Repeat("v", model.MaxLabelValueBytes), "a.b_c-1": "x:/@+-._1"}, nil, nil), nil); err != nil {
		t.Fatal("bounds rejected", err)
	}
	// The overlay bound counts removals and accumulates across changes.
	remove := []string{}
	for i := range model.MaxOwnerLabels - 2 {
		remove = append(remove, "r"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if err := a.JSON(ctx, "PATCH", path, labelPatch(nil, remove, nil), nil); err != nil {
		t.Fatal(err)
	}
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{"extra": "1"}, nil, nil), nil); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatal("overlay exceeded its bound", err)
	}
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{"extra": "1"}, nil, remove[:1]), nil); err != nil {
		t.Fatal("replacing an entry within the bound", err)
	}
}

// Only the fleet owner may set labels: its account key or browser session, or
// the superuser on the legacy fleet it already manages. Common keys,
// installation keys, other accounts and the superuser on another fleet cannot.
func TestOwnerLabelAuthorization(t *testing.T) {
	g, s := installationFixture(t)
	alice, a := createTestUser(t, s, "alice")
	_, b := createTestUser(t, s, "bob")
	ctx := context.Background()
	var issued struct {
		Key   APIKey
		Token string
	}
	if err := a.JSON(ctx, "POST", "/v1/fleet/keys", map[string]string{"name": "worker"}, &issued); err != nil {
		t.Fatal(err)
	}
	aliceCommon := testAdmin{URL: s.URL, Key: issued.Token}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	labelPeer(t, s.URL, issued.Token, "render", id, map[string]string{"zone": "lab"}, nil)

	// A redeemed installation key bound to another machine of this fleet.
	link := invitationFor(t, a, s.URL, "installed")
	installed, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ticket := link.URL[strings.LastIndex(link.URL, "/")+1:]
	q := enrollment.Redemption{PublicKey: installed.Public, CredentialHash: enrollment.Hash("installed-credential")}
	q.Signature = ed25519.Sign(installed.Private, enrollment.Message(ticket, q))
	if err := (testAdmin{URL: link.URL}).JSON(ctx, "POST", "/redeem", q, nil); err != nil {
		t.Fatal(err)
	}
	installation := testAdmin{URL: s.URL, Key: "installed-credential"}
	if err := installation.JSON(ctx, "GET", "/v1/nodes", nil, nil); err != nil {
		t.Fatal("installation credential does not authenticate", err)
	}

	path := "/v1/fleet/nodes/" + id.ID
	body := labelPatch(map[string]string{"pool": "stolen"}, nil, nil)
	for name, c := range map[string]struct {
		admin  testAdmin
		status string
	}{
		"common key":            {aliceCommon, "403"},
		"bootstrap common key":  {testAdmin{URL: s.URL, Key: commonKey}, "403"},
		"installation key":      {installation, "403"},
		"other account":         {b, "404"},
		"superuser other fleet": {testAdmin{URL: s.URL, Key: superKey}, "404"},
	} {
		if err := c.admin.JSON(ctx, "PATCH", path, body, nil); err == nil || !strings.Contains(err.Error(), c.status) {
			t.Fatalf("%s set labels: %v", name, err)
		}
	}
	if n := directoryNode(t, a, id.ID); n.Labels["pool"] != "" || countAudit(t, g, "machine.labels") != 0 {
		t.Fatal("unauthorized label change applied", n.Labels)
	}

	// The owner's browser session uses the console allowlist and CSRF rules.
	provisionBrowser(t, a, "")
	browser := browserLogin(t, s.URL, "alice", browserPassword)
	forged := browser
	forged.CSRF = "wrong"
	forged.request(t, "PATCH", "/console/api/fleet/nodes/"+id.ID, body, http.StatusForbidden)
	data, _ := browser.request(t, "PATCH", "/console/api/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "browser"}, nil, nil), http.StatusOK)
	if !strings.Contains(string(data), `"pool":"browser"`) {
		t.Fatal("browser label change", string(data))
	}
	var view model.MachineLabels
	if err := a.JSON(ctx, "PATCH", path, labelPatch(map[string]string{"pool": "owner"}, nil, nil), &view); err != nil || view.Labels["pool"] != "owner" {
		t.Fatal("account owner label change", view, err)
	}
	var events []AuditEvent
	rows, err := g.db.Query(`SELECT data FROM audit_events WHERE action='machine.labels' ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var e AuditEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	_ = rows.Close()
	if len(events) != 2 || events[0].Via != "browser" || events[0].UserID != alice.ID || events[1].Detail != "set pool=owner" || events[1].TargetID != id.ID {
		t.Fatal("label audit events", events)
	}

	// The superuser manages labels only on the legacy fleet.
	legacy, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	labelPeer(t, s.URL, commonKey, "legacy-node", legacy, nil, nil)
	if err := (testAdmin{URL: s.URL, Key: superKey}).JSON(ctx, "PATCH", "/v1/fleet/nodes/"+legacy.ID, labelPatch(map[string]string{"pool": "legacy"}, nil, nil), &view); err != nil || view.Labels["pool"] != "legacy" {
		t.Fatal("superuser legacy label change", view, err)
	}
	if err := a.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+legacy.ID, body, nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal("account changed legacy labels", err)
	}
}

func invitationFor(t *testing.T, a testAdmin, gatewayURL, name string) enrollment.Link {
	t.Helper()
	var link enrollment.Link
	if err := a.JSON(context.Background(), "POST", "/v1/fleet/installations", enrollment.Request{Name: name, OS: "linux", Arch: "amd64", Gateway: gatewayURL}, &link); err != nil {
		t.Fatal(err)
	}
	return link
}

// The overlay and its audit event commit in one transaction: a failed commit
// changes neither the cache nor the directory.
func TestOwnerLabelPersistenceFailureRollsBack(t *testing.T) {
	g, s := installationFixture(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	labelPeer(t, s.URL, commonKey, "render", id, map[string]string{"zone": "lab"}, nil)
	su := testAdmin{URL: s.URL, Key: superKey}
	ctx := context.Background()
	if err := su.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "a"}, nil, nil), nil); err != nil {
		t.Fatal(err)
	}
	// Fail the overlay write after the audit insert in the same transaction.
	if _, err := g.db.Exec(`CREATE TRIGGER fail_labels BEFORE UPDATE ON machine_labels BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	before := countAudit(t, g, "machine.labels")
	if err := su.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "b"}, []string{"zone"}, nil), nil); err == nil {
		t.Fatal("failed label commit acknowledged")
	}
	if countAudit(t, g, "machine.labels") != before {
		t.Fatal("audit event survived a rolled back label change")
	}
	g.mu.Lock()
	overlay := g.labelOverlays[id.ID]
	g.mu.Unlock()
	if overlay.Set["pool"] != "a" || len(overlay.Remove) != 0 {
		t.Fatal("failed change altered the cache", overlay)
	}
	if n := directoryNode(t, su, id.ID); n.Labels["pool"] != "a" || n.Labels["zone"] != "lab" {
		t.Fatal("failed change was published", n.Labels)
	}
}

// Unregistering a machine removes its overlay with the registration.
func TestOwnerLabelsRemovedWithRegistration(t *testing.T) {
	g, s := installationFixture(t)
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ws := labelPeer(t, s.URL, commonKey, "render", id, nil, nil)
	su := testAdmin{URL: s.URL, Key: superKey}
	ctx := context.Background()
	if err := su.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "a"}, nil, nil), nil); err != nil {
		t.Fatal(err)
	}
	_ = ws.CloseNow()
	if err := su.JSON(ctx, "DELETE", "/v1/fleet/nodes/"+id.ID+"?stop=true", nil, nil); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := g.db.QueryRow(`SELECT count(*) FROM machine_labels WHERE id=?`, id.ID).Scan(&rows); err != nil || rows != 0 {
		t.Fatal("overlay retained after unregistration", rows, err)
	}
	g.mu.Lock()
	_, cached := g.labelOverlays[id.ID]
	g.mu.Unlock()
	if cached {
		t.Fatal("overlay cached after unregistration")
	}
	if err := su.JSON(ctx, "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "a"}, nil, nil), nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal("labels set on an unregistered machine", err)
	}
}

// Schema version 8 databases gain machine_labels transactionally, keeping
// existing registrations; executables older than version 9 refuse it.
func TestOwnerLabelSchemaMigration(t *testing.T) {
	dir := t.TempDir()
	g, err := New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal(err)
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	g.owners[id.ID], g.nodes[id.ID] = legacyUser, model.Node{ID: id.ID, PublicKey: id.Public, UserID: legacyUser, Name: "existing", Labels: map[string]string{"zone": "lab"}}
	if err := g.persist(); err != nil {
		t.Error(err)
	}
	g.mu.Unlock()
	_ = g.Close()
	db, err := sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TABLE machine_labels; PRAGMA user_version=8;`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	g, err = New(dir, commonKey, Options{SuperuserKey: superKey})
	if err != nil {
		t.Fatal("migrate version 8", err)
	}
	var version int
	if err := g.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatal("schema version", version, err)
	}
	s := httptest.NewServer(g.Handler())
	su := testAdmin{URL: s.URL, Key: superKey}
	var view model.MachineLabels
	if err := su.JSON(context.Background(), "PATCH", "/v1/fleet/nodes/"+id.ID, labelPatch(map[string]string{"pool": "a"}, nil, nil), &view); err != nil || view.Labels["zone"] != "lab" || view.Labels["pool"] != "a" {
		t.Fatal("labels after migration", view, err)
	}
	s.Close()
	_ = g.Close()
	db, err = sql.Open("sqlite", filepath.Join(dir, "gateway.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(fmt.Sprintf(`PRAGMA user_version=%d;`, currentSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if g, err = New(dir, commonKey, Options{SuperuserKey: superKey}); err == nil {
		_ = g.Close()
		t.Fatal("newer schema accepted")
	}
}

func TestAuthAdvertisesMachineLabels(t *testing.T) {
	_, s := installationFixture(t)
	var auth map[string]any
	if err := (testAdmin{URL: s.URL, Key: commonKey}).JSON(context.Background(), "GET", "/v1/auth", nil, &auth); err != nil || auth["machineLabels"] != true {
		t.Fatal("machineLabels advertisement", auth, err)
	}
}
