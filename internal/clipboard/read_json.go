//go:build darwin || windows

package clipboard

import (
	"encoding/json"
	"fmt"
	"os/exec"
)

func readJSON(cmd *exec.Cmd) (Value, error) {
	b, err := output(cmd)
	if err != nil {
		return Value{}, err
	}
	var value Value
	if err := json.Unmarshal(b, &value); err != nil {
		return Value{}, fmt.Errorf("decode clipboard: %w", err)
	}
	return value, nil
}
