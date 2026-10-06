package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/koltyakov/control/internal/model"
	"github.com/koltyakov/control/internal/rpa"
)

func (n *Node) registerRPA() error {
	if n.Config.RPA == nil {
		return nil
	}
	if n.Config.RPA.Command == "" {
		return errors.New("rpa requires a desktop helper command")
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("RPA desktop lock directory: %w", err)
	}
	n.rpaLockPath = filepath.Join(dir, "control", "rpa.lock")
	return n.Register(provider{cap: model.Capability{
		Name: "rpa.run", InputSchema: model.JSON(rpa.Schema()),
		Description: "Run a serialized GUI action batch in this node user's desktop session. Inspect accessibility elements before exact selector actions; screenshots return PNG artifacts. Coordinate input requires X11 on Linux. Use leased tasks across multiple batches. Never automatically replay failed actions.",
	}, run: n.rpaRun})
}

type rpaResponse struct {
	Results []map[string]json.RawMessage `json:"results"`
	Error   string                       `json:"error,omitempty"`
}

func (n *Node) rpaRun(ctx context.Context, args json.RawMessage, e Execution) (any, error) {
	if err := rpa.Validate(args); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := os.MkdirAll(filepath.Dir(n.rpaLockPath), 0700); err != nil {
		return nil, err
	}
	lock := flock.New(n.rpaLockPath)
	defer func() { _ = lock.Close() }()
	locked, err := lock.TryLockContext(ctx, 50*time.Millisecond)
	if err != nil {
		return nil, fmt.Errorf("waiting for desktop: %w", err)
	}
	if !locked {
		return nil, errors.New("desktop is busy")
	}
	// Each invocation owns a private directory. Helpers never receive a caller-
	// supplied screenshot path, and bulk images never enter their JSON stdout.
	dir, err := os.MkdirTemp(n.Config.DataDir, "rpa-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	input := string(model.JSON(map[string]any{"version": model.Version, "arguments": args, "workspace": dir})) + "\n"
	result, err := runCommand(ctx, *n.Config.RPA, e, input)
	if err != nil {
		return nil, fmt.Errorf("RPA helper failed; desktop effects may already have occurred: %w; %s", err, result.Stderr)
	}
	if result.Truncated {
		return nil, errors.New("RPA helper output exceeds 2 MiB; desktop effects may already have occurred")
	}
	var response rpaResponse
	if err := json.Unmarshal([]byte(result.Stdout), &response); err != nil {
		return nil, fmt.Errorf("invalid RPA helper response; desktop effects may already have occurred: %w", err)
	}
	var request struct {
		Actions []json.RawMessage `json:"actions"`
	}
	_ = json.Unmarshal(args, &request) // Validated above.
	if response.Results == nil || len(response.Results) > len(request.Actions) || (response.Error == "" && len(response.Results) != len(request.Actions)) {
		return nil, errors.New("RPA helper returned an invalid action count; desktop effects may already have occurred")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	for _, item := range response.Results {
		if item == nil {
			return nil, errors.New("RPA helper results must be objects")
		}
		if raw, ok := item["image"]; ok {
			var name string
			if err := json.Unmarshal(raw, &name); err != nil || name == "" || filepath.Base(name) != name || name == "." || name == ".." {
				return response, errors.New("RPA helper returned an invalid screenshot path")
			}
			artifact, err := n.rpaImage(ctx, root, name)
			if err != nil {
				return response, err
			}
			delete(item, "image")
			item["artifact"] = model.JSON(artifact)
		}
	}
	if response.Error != "" {
		return response, fmt.Errorf("RPA stopped after %d completed actions; do not replay automatically: %s", len(response.Results), response.Error)
	}
	return response, nil
}

func (n *Node) rpaImage(ctx context.Context, root *os.Root, name string) (model.Artifact, error) {
	f, err := root.Open(name)
	if err != nil {
		return model.Artifact{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return model.Artifact{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 32<<20 {
		return model.Artifact{}, errors.New("RPA screenshot must be a regular PNG file of at most 32 MiB")
	}
	image, err := png.DecodeConfig(f)
	if err != nil || image.Width < 1 || image.Height < 1 || int64(image.Width)*int64(image.Height) > 64_000_000 {
		return model.Artifact{}, errors.New("invalid RPA PNG or screenshot exceeds 64 million pixels")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return model.Artifact{}, err
	}
	return n.importArtifact(ctx, io.LimitReader(f, (32<<20)+1), name)
}
