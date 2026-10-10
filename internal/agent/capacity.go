package agent

import (
	"context"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/quantity"

	"go.uber.org/zap"
)

// ResourceProber reports the node's static resource totals: the CPU count and
// memory available to containers. docker.DockerRuntime implements it from
// `docker info`, which on Docker Desktop reflects the VM's limits rather than
// the host's, the right denominator for placement.
type ResourceProber interface {
	NodeResources(ctx context.Context) (cpuCores int, memoryBytes int64, err error)
}

// SystemReserved is the node capacity held back for the OS, dockerd and the
// agent itself, and therefore never offered to Projects.
type SystemReserved struct {
	CPUMilli    int64
	MemoryBytes int64
}

// nodeCapacity builds the Capacity and Allocatable resource lists for a node
// with the given totals. Allocatable is Capacity minus reserved, floored at
// zero so a node smaller than the reservation reports nothing allocatable
// rather than a negative amount.
func nodeCapacity(cpuCores int, memoryBytes int64, reserved SystemReserved) (capacity, allocatable v1.ResourceList) {
	cpuMilli := int64(cpuCores) * 1000
	capacity = v1.ResourceList{
		"cpu":    quantity.FormatCPU(cpuMilli),
		"memory": quantity.FormatMemory(memoryBytes),
	}
	allocatable = v1.ResourceList{
		"cpu":    quantity.FormatCPU(max(cpuMilli-reserved.CPUMilli, 0)),
		"memory": quantity.FormatMemory(max(memoryBytes-reserved.MemoryBytes, 0)),
	}
	return capacity, allocatable
}

// probeNodeCapacity measures the node and returns its Capacity and
// Allocatable. It returns nil lists when prober is nil or the measurement
// fails; the heartbeat then omits both fields and the server keeps the last
// reported values, which beats reporting a guess.
func probeNodeCapacity(ctx context.Context, prober ResourceProber, reserved SystemReserved, logger *zap.Logger) (capacity, allocatable v1.ResourceList) {
	if prober == nil {
		return nil, nil
	}
	cpuCores, memoryBytes, err := prober.NodeResources(ctx)
	if err != nil {
		logger.Warn("Failed to measure node capacity; heartbeat will omit it", zap.Error(err))
		return nil, nil
	}
	return nodeCapacity(cpuCores, memoryBytes, reserved)
}
