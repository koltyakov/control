package client

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestDelegationCleanupRevokesPastUnreachableDestination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		grants := []model.Delegation{{ID: "stuck", Target: "down"}}
		for i := range 20 {
			grants = append(grants, model.Delegation{ID: fmt.Sprint("grant-", i), Target: "up"})
		}
		var mu sync.Mutex
		revoked := map[string]bool{}
		started := time.Now()
		revokeDelegations(t.Context(), grants, func(ctx context.Context, grant model.Delegation) error {
			if grant.Target == "down" {
				<-ctx.Done()
				return ctx.Err()
			}
			mu.Lock()
			revoked[grant.ID] = true
			mu.Unlock()
			return nil
		})
		if len(revoked) != len(grants)-1 {
			t.Fatal("unreachable destination left other grants unrevoked", len(revoked))
		}
		if elapsed := time.Since(started); elapsed > revokeTimeout {
			t.Fatal("cleanup waited beyond one revoke deadline", elapsed)
		}
	})
}
