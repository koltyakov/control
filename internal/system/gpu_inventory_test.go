package system

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/koltyakov/control/internal/model"
)

func TestGPUInventoryRetriesAfterFailedFirstSample(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		g := gpuCollector{inventory: func(context.Context) ([]gpuDevice, error) {
			calls++
			if calls == 1 {
				return nil, errors.New("probe timed out")
			}
			return []gpuDevice{{info: model.GPUInfo{Name: "Test GPU", Vendor: "AMD", Kind: model.GPUDiscrete}}}, nil
		}}
		if len(g.sample(t.Context())) != 0 || g.initialized {
			t.Fatal("failed inventory was latched")
		}
		g.sample(t.Context())
		if calls != 1 {
			t.Fatal("failed inventory retried without backoff")
		}
		time.Sleep(gpuInventoryRetry)
		g.sample(t.Context())
		if calls != 2 || !g.initialized || len(g.devices) != 1 {
			t.Fatal("inventory was not retried after backoff", calls)
		}
		g.sample(t.Context())
		if calls != 2 {
			t.Fatal("successful inventory was repeated")
		}
	})
}
