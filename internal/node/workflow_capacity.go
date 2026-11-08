package node

import (
	"context"
	"sync"
	"time"

	"github.com/koltyakov/control/internal/model"
)

const (
	workflowReadyLookahead  = 32
	workflowCapacityTargets = 8
	workflowCapacityTimeout = time.Second
)

func workflowReadySteps(ordered []WorkflowStep, started, succeeded map[string]bool) []WorkflowStep {
	steps := make([]WorkflowStep, 0, workflowReadyLookahead)
	for _, step := range ordered {
		if !started[step.Name] && ready(step, succeeded) {
			steps = append(steps, step)
			if len(steps) == workflowReadyLookahead {
				break
			}
		}
	}
	return steps
}

// Probe each selected destination once per refill, concurrently under one
// deadline. Failed/legacy observations stay unknown; discovery grants no work.
func workflowCapacityReports(ctx context.Context, steps []WorkflowStep, read func(context.Context, string) *model.NodeConcurrency) map[string]*model.NodeConcurrency {
	ctx, cancel := context.WithTimeout(ctx, workflowCapacityTimeout)
	defer cancel()
	reports := map[string]*model.NodeConcurrency{}
	for _, step := range steps {
		if len(reports) == workflowCapacityTargets {
			break
		}
		reports[step.Target] = nil
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	targets := make([]string, 0, len(reports))
	for target := range reports {
		targets = append(targets, target)
	}
	for _, target := range targets {
		wg.Go(func() {
			report := read(ctx, target)
			mu.Lock()
			reports[target] = report
			mu.Unlock()
		})
	}
	wg.Wait()
	return reports
}

func (n *Node) workflowCapacity(ctx context.Context, target string) *model.NodeConcurrency {
	if target == "" || target == n.Identity.ID || target == n.Config.Name {
		// Observe our own counters without granting local coordinated execution.
		report := n.concurrencySnapshot()
		return &report
	}
	var description struct {
		Concurrency *model.NodeConcurrency `json:"concurrency"`
	}
	if err := n.Call(ctx, target, "node.describe", map[string]any{}, &description); err != nil {
		return nil
	}
	return description.Concurrency
}

func workflowProviderPool(capability string) string {
	switch capability {
	case "workflow.run", "peers.call":
		return ""
	case "files.list", "files.read", "files.write", "http.request", "network.wake":
		return "operations"
	case "desktop.run":
		// Descriptions do not distinguish built-in adapters from custom
		// providers using these names. Selection checks execution as well.
		return "desktop"
	default:
		return "execution"
	}
}

func workflowPool(report *model.NodeConcurrency, pool string) *model.ConcurrencyPool {
	if report == nil {
		return nil
	}
	switch pool {
	case "execution":
		return &report.Execution
	case "operations":
		return &report.Operations
	case "desktop":
		return report.Desktop
	case "inputStaging":
		return report.InputStaging
	}
	return nil
}

func workflowUsesPool(step WorkflowStep, pool string) bool {
	if pool == "inputStaging" {
		return len(step.Inputs) > 0 || len(step.Task.Inputs) > 0
	}
	provider := workflowProviderPool(step.Task.Capability)
	return pool == provider || provider == "desktop" && pool == "execution"
}

// Prefer available, then unknown, then busy. Equal ranks keep plan order.
// In-flight children conservatively consume headroom for their entire lifetime;
// reports may already count them, but can also predate their admission.
func workflowNextStep(steps []WorkflowStep, reports map[string]*model.NodeConcurrency, active map[string]WorkflowStep) int {
	best, bestRank := 0, 3
	for i, step := range steps {
		pool := workflowProviderPool(step.Task.Capability)
		pools := []string{}
		if pool != "" {
			pools = append(pools, pool)
		}
		if pool == "desktop" {
			// Either pool can admit this name, depending on destination config.
			// Require headroom in both rather than inventing adapter metadata.
			pools = append(pools, "execution")
		}
		if len(step.Inputs) > 0 || len(step.Task.Inputs) > 0 {
			pools = append(pools, "inputStaging")
		}
		rank := 0
		for _, name := range pools {
			reading := workflowPool(reports[step.Target], name)
			if reading == nil || reading.Limit < 1 || reading.InUse < 0 || reading.InUse > reading.Limit || reading.Queued == nil || *reading.Queued < 0 {
				rank = max(rank, 1)
				continue
			}
			pending := 0
			for _, child := range active {
				if child.Target == step.Target && workflowUsesPool(child, name) {
					pending++
				}
			}
			if pending >= reading.Limit-reading.InUse || *reading.Queued > 0 {
				rank = 2
			}
		}
		if rank < bestRank {
			best, bestRank = i, rank
		}
	}
	return best
}
