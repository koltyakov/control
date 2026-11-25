package node

import (
	"path/filepath"

	"github.com/koltyakov/control/internal/secrets"
)

func (n *Node) secretStore() secrets.Store {
	return secrets.Store{Dir: filepath.Join(n.Config.DataDir, "secrets"), WorkDir: n.Config.WorkDir}
}

// secretValues returns every configured value for redaction and storage
// validation only. Values injected or sent anywhere must come from
// secrets.Resolver.Resolve, which enforces each secret's use policy;
// TestSecretValueCallSites keeps this list of call sites closed.
func (n *Node) secretValues() (map[string]string, error) {
	return n.secretStore().Snapshot()
}

// secretResolver reads the store once for one invocation, so masking and
// resolved references come from the same snapshot.
func (n *Node) secretResolver() (*secrets.Resolver, error) {
	return n.secretStore().Resolver()
}
