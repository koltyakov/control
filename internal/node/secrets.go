package node

import (
	"errors"
	"path/filepath"

	"github.com/koltyakov/control/internal/secrets"
)

func (n *Node) secretValues() (map[string]string, error) {
	return (secrets.Store{Dir: filepath.Join(n.Config.DataDir, "secrets"), WorkDir: n.Config.WorkDir}).Snapshot()
}

func resolveSecret(values map[string]string, name string) (string, error) {
	if !secrets.ValidName(name) {
		return "", errors.New("invalid secret reference")
	}
	value, ok := values[name]
	if !ok {
		return "", errors.New("referenced secret is not configured on this worker")
	}
	return value, nil
}
