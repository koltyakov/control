package node

import (
	"encoding/json"
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

func resolveRPASecrets(args json.RawMessage, values map[string]string) (json.RawMessage, error) {
	var request struct {
		Actions []map[string]any `json:"actions"`
	}
	if err := json.Unmarshal(args, &request); err != nil {
		return nil, err
	}
	for _, action := range request.Actions {
		if name, ok := action["secret"].(string); ok {
			value, err := resolveSecret(values, name)
			if err != nil {
				return nil, err
			}
			action["text"] = value
			delete(action, "secret")
		}
	}
	return json.Marshal(request)
}
