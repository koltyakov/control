package clipboard

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const MaxText = 1 << 20

// Value is local clipboard data. Paths never travel to the receiving machine.
type Value struct {
	Text  string   `json:"text"`
	Paths []string `json:"paths"`
}

// Read prefers file references over text. Reading references does not read file bytes.
func Read(ctx context.Context) (Value, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return read(ctx)
}

type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 2*MaxText {
		return 0, errors.New("clipboard output exceeds 2 MiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

func output(cmd *exec.Cmd) ([]byte, error) {
	cmd.WaitDelay = time.Second
	b := &boundedOutput{}
	cmd.Stdout = b
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("read clipboard: %w", err)
	}
	return b.data, nil
}
