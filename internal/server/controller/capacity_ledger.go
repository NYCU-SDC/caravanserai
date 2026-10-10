package controller

import (
	"context"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/quantity"

	"go.uber.org/zap"
)

// CapacityProjectStore is the store surface the CapacityLedger needs to learn
// what is placed on a Node.
type CapacityProjectStore interface {
	// ListProjectsByNodeRef returns all Projects assigned to nodeRef whose
	// phase is one of phases.
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*ProjectSnapshot, error)
}

// ledgerPhases are the phases whose Projects hold resources on their Node.
// Terminating counts because its containers are not yet removed. Failed does
// not: its containers are stopped and it is never restarted in place, so
// counting it would pin capacity until someone deletes the Project.
var ledgerPhases = []v1.ProjectPhase{
	v1.ProjectPhaseScheduled,
	v1.ProjectPhaseRunning,
	v1.ProjectPhaseTerminating,
}

// NodeUsage is the CPU and memory declared by the Projects placed on a Node.
type NodeUsage struct {
	CPUMilli    int64
	MemoryBytes int64
}

// CapacityLedger is the static capacity ledger of
// docs/scheduler-strategy.md §3.4. It books each Project's declared size
// against a Node's Allocatable; it never measures live usage, so a Medium
// Project always books a Medium amount.
//
//	fits  <=>  Σ(sizes already placed on the Node) + new size <= Allocatable
//
// Allocatable already excludes the agent's system-reserved headroom, so the
// ledger holds back nothing further.
type CapacityLedger struct {
	logger   *zap.Logger
	projects CapacityProjectStore
}

// NewCapacityLedger creates a CapacityLedger.
func NewCapacityLedger(logger *zap.Logger, projects CapacityProjectStore) *CapacityLedger {
	return &CapacityLedger{logger: logger, projects: projects}
}

// Used returns the resources booked by the Projects placed on the named Node.
func (l *CapacityLedger) Used(ctx context.Context, node string) (NodeUsage, error) {
	projects, err := l.projects.ListProjectsByNodeRef(ctx, node, ledgerPhases)
	if err != nil {
		return NodeUsage{}, fmt.Errorf("list projects on node %q: %w", node, err)
	}

	var used NodeUsage
	for _, p := range projects {
		req := p.Size.Requests()
		used.CPUMilli += req.CPUMilli
		used.MemoryBytes += req.MemoryBytes
	}
	return used, nil
}

// Fits reports whether a Project of the given size can be placed on node
// without the Node's booked total exceeding its Allocatable. When it cannot,
// reason says why, for the scheduler to log; the error is non-nil only when
// the ledger could not be computed.
//
// A Node that has not reported Allocatable, or reports one that cannot be
// parsed, does not fit: placing onto it would be unguarded over-commit.
func (l *CapacityLedger) Fits(ctx context.Context, node ReadyNode, size v1.ProjectSize) (ok bool, reason string, err error) {
	cpuMilli, memoryBytes, reason := parseAllocatable(node)
	if reason != "" {
		return false, reason, nil
	}

	used, err := l.Used(ctx, node.Name)
	if err != nil {
		return false, "", err
	}

	req := size.Requests()
	if used.CPUMilli+req.CPUMilli > cpuMilli {
		return false, fmt.Sprintf("node %q lacks cpu: %dm in use + %dm requested > %dm allocatable",
			node.Name, used.CPUMilli, req.CPUMilli, cpuMilli), nil
	}
	if used.MemoryBytes+req.MemoryBytes > memoryBytes {
		return false, fmt.Sprintf("node %q lacks memory: %s in use + %s requested > %s allocatable",
			node.Name, quantity.FormatMemory(used.MemoryBytes), quantity.FormatMemory(req.MemoryBytes),
			quantity.FormatMemory(memoryBytes)), nil
	}
	return true, "", nil
}

// parseAllocatable reads the node's Allocatable cpu and memory. A non-empty
// reason means it could not, and says whether the value was missing (the agent
// predates CARA-111) or malformed.
func parseAllocatable(node ReadyNode) (cpuMilli, memoryBytes int64, reason string) {
	var missing []string
	cpu, hasCPU := node.Allocatable["cpu"]
	mem, hasMem := node.Allocatable["memory"]
	if !hasCPU {
		missing = append(missing, "cpu")
	}
	if !hasMem {
		missing = append(missing, "memory")
	}
	if len(missing) > 0 {
		return 0, 0, fmt.Sprintf("node %q has not reported allocatable %v; upgrade cara-agent", node.Name, missing)
	}

	cpuMilli, err := quantity.ParseCPU(cpu)
	if err != nil {
		return 0, 0, fmt.Sprintf("node %q reports unparseable allocatable cpu: %v", node.Name, err)
	}
	memoryBytes, err = quantity.ParseMemory(mem)
	if err != nil {
		return 0, 0, fmt.Sprintf("node %q reports unparseable allocatable memory: %v", node.Name, err)
	}
	return cpuMilli, memoryBytes, ""
}
