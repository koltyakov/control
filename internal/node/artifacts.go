package node

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/identity"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/store"
)

var digestID = regexp.MustCompile(`^[a-f0-9]{64}$`)

func (n *Node) artifactPath(id string) string {
	return filepath.Join(n.Config.DataDir, "artifacts", id)
}

// storedArtifact is the local metadata record. Owners lists the task owners
// holding the content; an empty list is a shared record from an older version
// or one with more owners than are tracked.
type storedArtifact struct {
	model.Artifact
	Owners []string `json:"owners,omitempty"`
}

const maxArtifactOwners = 256

func (n *Node) artifact(id string) (model.Artifact, error) {
	stored, err := n.storedArtifact(id)
	return stored.Artifact, err
}

func (n *Node) storedArtifact(id string) (storedArtifact, error) {
	var a storedArtifact
	if !digestID.MatchString(id) {
		return a, errors.New("invalid artifact ID")
	}
	err := store.Read(n.artifactPath(id)+".json", &a)
	return a, err
}

// visibleTo reports whether owner may list or remove its hold on the record.
func (a storedArtifact) visibleTo(owner, node string) bool {
	return owner == node || len(a.Owners) == 0 || slices.Contains(a.Owners, owner)
}

// artifactOwner is the task owner of the current invocation.
func (n *Node) artifactOwner(ctx context.Context) string {
	if meta, _ := ctx.Value(activityContextKey{}).(activityContext); meta.owner != "" {
		return meta.owner
	}
	return n.Identity.ID
}

// holdArtifactLocked records owner on stored content. Caller holds artifactMu.
func (n *Node) holdArtifactLocked(a storedArtifact, owner string) error {
	if len(a.Owners) == 0 || slices.Contains(a.Owners, owner) {
		return nil
	}
	if len(a.Owners) >= maxArtifactOwners {
		a.Owners = nil
	} else {
		a.Owners = append(a.Owners, owner)
	}
	return store.Write(n.artifactPath(a.ID)+".json", a)
}

type transferLock struct {
	sync.Mutex
	refs int
}

// lockTransfer serializes transfers of one artifact and forgets the lock once
// no transfer needs it.
func (n *Node) lockTransfer(id string) func() {
	n.artifactMu.Lock()
	lock := n.transfers[id]
	if lock == nil {
		lock = &transferLock{}
		n.transfers[id] = lock
	}
	lock.refs++
	n.artifactMu.Unlock()
	lock.Lock()
	return func() {
		lock.Unlock()
		n.artifactMu.Lock()
		if lock.refs--; lock.refs == 0 {
			delete(n.transfers, id)
		}
		n.artifactMu.Unlock()
	}
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func (n *Node) importArtifact(ctx context.Context, reader io.Reader, name string) (_ model.Artifact, err error) {
	activity := n.beginActivity(ctx, "transfer", "artifacts.import", "", "")
	defer func() { activity.finish(err) }()
	activity.phase("storing")
	if file, ok := reader.(*os.File); ok {
		if info, e := file.Stat(); e == nil {
			activity.progress(0, info.Size())
		}
	}
	dir := filepath.Join(n.Config.DataDir, "artifacts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return model.Artifact{}, err
	}
	f, err := os.CreateTemp(dir, ".import-*")
	if err != nil {
		return model.Artifact{}, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	defer func() { _ = f.Close() }()
	hash := sha256.New()
	size, err := io.Copy(activityWriter{writer: io.MultiWriter(f, hash), counter: &activity.bytes}, contextReader{ctx, reader})
	if err != nil {
		return model.Artifact{}, err
	}
	if err = f.Sync(); err != nil {
		return model.Artifact{}, err
	}
	if err = f.Close(); err != nil {
		return model.Artifact{}, err
	}
	id := hex.EncodeToString(hash.Sum(nil))
	owner := n.artifactOwner(ctx)
	n.artifactMu.Lock()
	defer n.artifactMu.Unlock()
	if existing, e := n.storedArtifact(id); e == nil {
		return existing.Artifact, n.holdArtifactLocked(existing, owner)
	}
	if err = os.Rename(f.Name(), n.artifactPath(id)); err != nil {
		return model.Artifact{}, err
	}
	a := model.Artifact{Node: n.Identity.ID, ID: id, Name: name, Size: size, SHA256: id, Created: time.Now().UTC()}
	return a, store.Write(n.artifactPath(id)+".json", storedArtifact{Artifact: a, Owners: []string{owner}})
}

type artifactGrant struct {
	Subject  string `json:"subject"`
	Artifact string `json:"artifact"`
	Expires  int64  `json:"expires"`
	Access   string `json:"access,omitempty"`
	TaskID   string `json:"taskId,omitempty"`
	Output   int    `json:"output,omitempty"`
}

func (n *Node) grantArtifact(a model.Artifact, subject string, ttl time.Duration) model.Artifact {
	body := model.JSON(artifactGrant{Subject: subject, Artifact: a.ID, Expires: time.Now().Add(ttl).Unix()})
	sig := ed25519.Sign(n.Identity.Private, body)
	a.Grant = base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(sig)
	return a
}

func (n *Node) validGrant(token, subject, artifact string) bool {
	var g artifactGrant
	return n.decodeGrant(token, &g) && g.Subject == subject && g.Artifact == artifact && g.Expires > time.Now().Unix()
}

func (n *Node) decodeGrant(token string, g *artifactGrant) bool {
	return decodeArtifactGrant(token, n.Identity.Public, g)
}

func decodeArtifactGrant(token string, public ed25519.PublicKey, g *artifactGrant) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	if !ed25519.Verify(public, body, sig) {
		return false
	}
	return json.Unmarshal(body, g) == nil
}

func (n *Node) serveArtifact(ctx context.Context, caller string, conn net.Conn, args json.RawMessage) {
	var q struct {
		ID     string `json:"id"`
		Offset int64  `json:"offset"`
		Grant  string `json:"grant"`
	}
	err := json.Unmarshal(args, &q)
	if err == nil && !n.validGrant(q.Grant, caller, q.ID) {
		err = n.authorize(ctx, caller, "artifacts.open")
	}
	var a model.Artifact
	if err == nil {
		a, err = n.artifact(q.ID)
	}
	if err == nil && (q.Offset < 0 || q.Offset > a.Size) {
		err = errors.New("invalid artifact offset")
	}
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	activity := n.beginActivity(ctx, "transfer", "artifacts.send", n.executionOwner(ctx, caller), caller)
	workCtx, release, gateErr := n.enterWork(ctx)
	if gateErr != nil {
		activity.finish(gateErr)
		_ = writeFrame(conn, model.Response{Error: gateErr.Error()})
		return
	}
	defer release()
	ctx = workCtx
	defer func() { activity.finish(err) }()
	activity.phase("sending")
	activity.progress(q.Offset, a.Size)
	f, err := os.Open(n.artifactPath(q.ID))
	if err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Seek(q.Offset, io.SeekStart); err != nil {
		_ = writeFrame(conn, model.Response{Error: err.Error()})
		return
	}
	if err = writeFrame(conn, model.Response{Result: model.JSON(a)}); err != nil {
		return
	}
	_, err = io.CopyN(activityWriter{writer: conn, counter: &activity.bytes}, contextReader{ctx, f}, a.Size-q.Offset)
}

func (n *Node) Download(ctx context.Context, a model.Artifact, offset int64, dest io.Writer) (err error) {
	workCtx, release, gateErr := n.enterWork(ctx)
	if gateErr != nil {
		return gateErr
	}
	defer release()
	ctx = workCtx
	activity := n.beginActivity(ctx, "transfer", "artifacts.download", "", a.Node)
	defer func() { activity.finish(err) }()
	activity.phase("receiving")
	activity.progress(offset, a.Size)
	dest = activityWriter{writer: dest, counter: &activity.bytes}
	if a.Node == n.Identity.ID || a.Node == n.Config.Name {
		stored, err := n.artifact(a.ID)
		if err != nil {
			return err
		}
		if offset < 0 || offset > stored.Size {
			return errors.New("invalid artifact offset")
		}
		f, err := os.Open(n.artifactPath(a.ID))
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		_, err = io.CopyN(dest, contextReader{ctx, f}, stored.Size-offset)
		return err
	}
	conn, data, err := n.open(ctx, a.Node, "artifacts.open", map[string]any{"id": a.ID, "offset": offset, "grant": a.Grant})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	var remote model.Artifact
	if err = json.Unmarshal(data, &remote); err != nil {
		return err
	}
	if remote.ID != a.ID || remote.SHA256 != a.ID || remote.Size != a.Size {
		return errors.New("artifact metadata mismatch")
	}
	_, err = io.CopyN(dest, conn, remote.Size-offset)
	return err
}

func (n *Node) pullArtifact(ctx context.Context, a model.Artifact) (_ model.Artifact, err error) {
	activity := n.beginActivity(ctx, "operation", "artifacts.pull", "", a.Node)
	defer func() { activity.finish(err) }()
	activity.phase("waiting for transfer slot")
	if !digestID.MatchString(a.ID) || a.SHA256 != a.ID || a.Size < 0 {
		return model.Artifact{}, errors.New("invalid artifact reference")
	}
	defer n.lockTransfer(a.ID)()
	owner := n.artifactOwner(ctx)
	activity.phase("checking local cache")
	n.artifactMu.Lock()
	local, err := n.storedArtifact(a.ID)
	if err == nil {
		err = n.holdArtifactLocked(local, owner)
		n.artifactMu.Unlock()
		return local.Artifact, err
	}
	n.artifactMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(n.artifactPath(a.ID)), 0700); err != nil {
		return model.Artifact{}, err
	}
	partial := n.artifactPath(a.ID) + ".partial"
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return model.Artifact{}, err
	}
	defer func() { _ = f.Close() }()
	offset, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return model.Artifact{}, err
	}
	if offset > a.Size {
		if err = f.Truncate(0); err != nil {
			return model.Artifact{}, err
		}
		offset = 0
	}
	// Hash while receiving so a completed download is not read a second time.
	// A resumed transfer hashes only its retained prefix first.
	hash := sha256.New()
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return model.Artifact{}, err
	}
	if offset > 0 {
		activity.phase("verifying resumed prefix")
		if _, err = io.CopyN(hash, contextReader{ctx, f}, offset); err != nil {
			return model.Artifact{}, err
		}
	}
	activity.phase("receiving")
	if err = n.Download(ctx, a, offset, io.MultiWriter(f, hash)); err != nil {
		return model.Artifact{}, fmt.Errorf("transfer paused at resumable partial: %w", err)
	}
	activity.phase("verifying checksum")
	if hex.EncodeToString(hash.Sum(nil)) != a.SHA256 {
		_ = f.Close()
		_ = os.Remove(partial)
		return model.Artifact{}, errors.New("artifact checksum mismatch")
	}
	if err = f.Sync(); err != nil {
		return model.Artifact{}, err
	}
	if err = f.Close(); err != nil {
		return model.Artifact{}, err
	}
	n.artifactMu.Lock()
	defer n.artifactMu.Unlock()
	if err = os.Rename(partial, n.artifactPath(a.ID)); err != nil {
		return model.Artifact{}, err
	}
	a.Node, a.Grant, a.Created = n.Identity.ID, "", time.Now().UTC()
	return a, store.Write(n.artifactPath(a.ID)+".json", storedArtifact{Artifact: a, Owners: []string{owner}})
}

func (n *Node) artifactMethod(ctx context.Context, caller, method string, args json.RawMessage) (any, error) {
	var q struct {
		ID          string         `json:"id"`
		Path        string         `json:"path"`
		Target      string         `json:"target"`
		Artifact    model.Artifact `json:"artifact"`
		TTLSeconds  int            `json:"ttlSeconds"`
		OutputIndex *int           `json:"outputIndex"`
	}
	if err := json.Unmarshal(args, &q); err != nil {
		return nil, err
	}
	switch method {
	case "artifacts.export":
		f, err := n.root.Open(q.Path)
		if err != nil {
			return nil, err
		}
		defer func() { _ = f.Close() }()
		if stat, e := f.Stat(); e != nil || !stat.Mode().IsRegular() {
			return nil, errors.New("artifact must be a regular file")
		}
		return n.importArtifact(ctx, f, filepath.Base(q.Path))
	case "artifacts.list":
		paths, err := filepath.Glob(filepath.Join(n.Config.DataDir, "artifacts", "*.json"))
		if err != nil {
			return nil, err
		}
		owner := n.artifactOwner(ctx)
		artifacts := []model.Artifact{}
		for _, path := range paths {
			var a storedArtifact
			if err = store.Read(path, &a); err != nil {
				return nil, err
			}
			if a.visibleTo(owner, n.Identity.ID) {
				artifacts = append(artifacts, a.Artifact)
			}
		}
		return artifacts, nil
	case "artifacts.delete":
		if !digestID.MatchString(q.ID) {
			return nil, errors.New("invalid artifact ID")
		}
		owner := n.artifactOwner(ctx)
		n.artifactMu.Lock()
		defer n.artifactMu.Unlock()
		stored, err := n.storedArtifact(q.ID)
		if err != nil {
			return nil, err
		}
		if !stored.visibleTo(owner, n.Identity.ID) {
			return nil, fmt.Errorf("open %s: %w", n.artifactPath(q.ID)+".json", os.ErrNotExist)
		}
		// Another owner's hold keeps the content; only this owner's is released.
		if owner != n.Identity.ID && len(stored.Owners) > 1 {
			stored.Owners = slices.DeleteFunc(stored.Owners, func(o string) bool { return o == owner })
			return nil, store.Write(n.artifactPath(q.ID)+".json", stored)
		}
		if err := os.Remove(n.artifactPath(q.ID) + ".json"); err != nil {
			return nil, err
		}
		return nil, os.Remove(n.artifactPath(q.ID))
	case "artifacts.pull":
		return n.pullArtifact(ctx, q.Artifact)
	case "artifacts.grant", "artifacts.deliver":
		if caller == n.Identity.ID {
			if _, ok := ctx.Value(authorityContextKey{}).(authority); !ok {
				return nil, errors.New("artifact delegation requires an orchestrator instruction")
			}
		}
		a, err := n.artifact(q.ID)
		if err != nil {
			return nil, err
		}
		peer, err := n.Peer.Resolve(ctx, q.Target)
		if err != nil {
			return nil, err
		}
		if q.TTLSeconds == 0 {
			q.TTLSeconds = 3600
		}
		if q.TTLSeconds < 1 || q.TTLSeconds > 86400 {
			return nil, errors.New("grant TTL must be 1..86400 seconds")
		}
		grantParent := n.ctx
		if a, ok := ctx.Value(authorityContextKey{}).(authority); ok && a.grant != nil {
			grantParent = a.grant.ctx
		}
		grantCtx, cancel := context.WithCancel(grantParent)
		g := model.Delegation{ID: identity.NewID(), Target: n.Identity.ID, Subject: peer.ID, Owner: n.executionOwner(ctx, caller), Method: "artifacts.open", Params: model.JSON(map[string]any{"id": a.ID}), Expires: time.Now().Add(delegationIdleTimeout).UTC()}
		n.delegationMu.Lock()
		if len(n.delegations) >= 4096 {
			n.delegationMu.Unlock()
			cancel()
			return nil, errors.New("delegation limit reached")
		}
		n.delegations[g.ID] = &delegationState{grant: g, ctx: grantCtx, cancel: cancel, lastActivity: time.Now()}
		n.delegationMu.Unlock()
		proof := artifactGrant{Subject: peer.ID, Artifact: a.ID, Expires: time.Now().Add(time.Duration(q.TTLSeconds) * time.Second).Unix(), Access: g.ID}
		if auth, ok := ctx.Value(authorityContextKey{}).(authority); ok && auth.grant != nil && auth.grant.grant.TaskID() != "" {
			proof.TaskID = auth.grant.grant.TaskID()
			n.mu.Lock()
			if task := n.tasks[proof.TaskID]; task != nil {
				for index, output := range task.Artifacts {
					if output.ID == a.ID && (q.OutputIndex == nil || *q.OutputIndex == index) {
						proof.Output = index
						break
					}
				}
			}
			n.mu.Unlock()
		}
		body := model.JSON(proof)
		a.Grant = base64.RawURLEncoding.EncodeToString(body) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(n.Identity.Private, body))
		if method == "artifacts.grant" {
			return a, nil
		}
		var delivered model.Artifact
		err = n.Call(ctx, peer.ID, "artifacts.pull", map[string]any{"artifact": a}, &delivered)
		return delivered, err
	}
	return nil, errors.New("unknown artifact method")
}
