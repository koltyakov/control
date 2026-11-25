package client

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/koltyakov/control/internal/model"
)

type fakeWakeVault struct {
	requests []model.FleetVaultCheck
	allowed  [][]string
	err      error
}

func (f *fakeWakeVault) IdentityID() string { return "client-transport" }

func (f *fakeWakeVault) CheckFleetVault(_ context.Context, q model.FleetVaultCheck) (model.FleetVaultDecision, error) {
	f.requests = append(f.requests, q)
	if f.err != nil {
		return model.FleetVaultDecision{}, f.err
	}
	for _, set := range f.allowed {
		covered := true
		for _, id := range q.MachineIDs {
			covered = covered && slices.Contains(set, id)
		}
		if covered {
			return model.FleetVaultDecision{Allowed: true}, nil
		}
	}
	return model.FleetVaultDecision{}, nil
}

// A local send asks once for the client's own transport identity, the target
// and the registered sender together; a refusal comes before any packet.
func TestLocalWakeFleetVault(t *testing.T) {
	sent := 0
	send := func(peer fleetVaultChecker, supported bool, machines ...string) error {
		err := localWakeAllowed(t.Context(), peer, supported, machines...)
		if err == nil {
			sent++
		}
		return err
	}
	// The sender alone is allowed, not the excluded target.
	vault := &fakeWakeVault{allowed: [][]string{{"sender"}}}
	err := send(vault, true, "target", "sender")
	var rpc *model.RPCError
	if !errors.As(err, &rpc) || rpc.Code != model.FleetVaultDenied || rpc.Outcome != model.OutcomeNotAccepted {
		t.Fatal("excluded target admitted", err)
	}
	if len(vault.requests) != 1 || vault.requests[0].Issuer != "client-transport" || !slices.Equal(vault.requests[0].MachineIDs, []string{"sender", "target"}) {
		t.Fatal("local wake check", vault.requests)
	}
	// Separate allowances for sender and target do not combine.
	vault = &fakeWakeVault{allowed: [][]string{{"sender"}, {"target"}}}
	if err := send(vault, true, "target", "sender"); !errors.As(err, &rpc) || rpc.Code != model.FleetVaultDenied {
		t.Fatal("blended allowances admitted", err)
	}
	// An unregistered local sender implicates only the target.
	if err := send(vault, true, "target"); err != nil {
		t.Fatal(err)
	}
	// Unavailability sends nothing and is asked once.
	vault = &fakeWakeVault{err: errors.New("offline")}
	if err := send(vault, true, "target"); !errors.As(err, &rpc) || rpc.Code != model.FleetVaultUnavailable || len(vault.requests) != 1 {
		t.Fatal("unavailable decision", err, vault.requests)
	}
	// A gateway without the feature is not asked.
	vault = &fakeWakeVault{}
	if err := send(vault, false, "target", "sender"); err != nil || len(vault.requests) != 0 {
		t.Fatal("pre-feature gateway", err, vault.requests)
	}
	if sent != 2 {
		t.Fatal("sends", sent)
	}
}
