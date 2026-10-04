package controller

import (
	"context"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// candidate is a Node that passed Filter, with the usage its capacity check
// was decided on so that Score does not recompute it.
type candidate struct {
	ReadyNode

	// Used is what the Projects already placed on the Node book.
	Used NodeUsage

	// Allocatable is what the Node offers to Projects.
	Allocatable NodeUsage
}

// filterNodes is the Filter stage of the scheduler (docs/scheduler-strategy.md
// §5.1). It narrows nodes, already limited to Ready and schedulable, to the
// candidates that can host a Project of the given size:
//
//   - the Node is not reporting DiskPressure or MemoryPressure, and
//   - the Project's size fits in what the Node has left (CapacityLedger).
//
// rejected has one line per excluded Node saying why, for the scheduler to log.
// The error is non-nil only when the ledger could not be computed; the
// scheduler then retries rather than treating the Node as full.
func filterNodes(ctx context.Context, ledger *CapacityLedger, nodes []ReadyNode, size v1.ProjectSize) (candidates []candidate, rejected []string, err error) {
	for _, n := range nodes {
		if cond, ok := underPressure(n); ok {
			rejected = append(rejected, fmt.Sprintf("node %q reports %s", n.Name, cond))
			continue
		}
		c, err := ledger.check(ctx, n, size)
		if err != nil {
			return nil, nil, err
		}
		if !c.ok {
			rejected = append(rejected, c.reason)
			continue
		}
		candidates = append(candidates, candidate{ReadyNode: n, Used: c.used, Allocatable: c.allocatable})
	}
	return candidates, rejected, nil
}

// underPressure reports whether the Node has a pressure condition that is
// True, and which. A condition that is False or Unknown does not exclude the
// Node: Unknown means nothing has sampled it, and excluding on it would leave
// every Node out of rotation until a producer exists.
func underPressure(n ReadyNode) (v1.ConditionType, bool) {
	for _, c := range n.Conditions {
		switch c.Type {
		case v1.ConditionTypeDiskPressure, v1.ConditionTypeMemoryPressure:
			if c.Status == v1.ConditionTrue {
				return c.Type, true
			}
		}
	}
	return "", false
}
