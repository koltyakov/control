package node

import (
	"context"
	"sync/atomic"

	"github.com/koltyakov/control/internal/model"
)

// commandUsageSink carries one tracked task's command accounting from the
// provider to its terminal record. Configured subprocess providers return
// their own JSON, so the record is the only place their usage appears.
type commandUsageSink struct {
	usage atomic.Pointer[model.CommandUsage]
}

type commandUsageKey struct{}

func withCommandUsage(ctx context.Context) context.Context {
	return context.WithValue(ctx, commandUsageKey{}, &commandUsageSink{})
}

func reportCommandUsage(ctx context.Context, usage *model.CommandUsage) {
	if sink, ok := ctx.Value(commandUsageKey{}).(*commandUsageSink); ok && usage != nil {
		sink.usage.Store(usage)
	}
}

func taskCommandUsage(ctx context.Context) *model.CommandUsage {
	if sink, ok := ctx.Value(commandUsageKey{}).(*commandUsageSink); ok {
		return sink.usage.Load()
	}
	return nil
}
