package agent

import (
	"context"
	"errors"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

type fakeProber struct {
	cpuCores    int
	memoryBytes int64
	err         error
}

func (f fakeProber) NodeResources(context.Context) (int, int64, error) {
	return f.cpuCores, f.memoryBytes, f.err
}

func TestNodeCapacity(t *testing.T) {
	defaultReserved := SystemReserved{CPUMilli: 500, MemoryBytes: 512 << 20}

	tests := []struct {
		name            string
		cpuCores        int
		memoryBytes     int64
		reserved        SystemReserved
		wantCapacity    v1.ResourceList
		wantAllocatable v1.ResourceList
	}{
		{
			name:            "typical node",
			cpuCores:        4,
			memoryBytes:     8 << 30,
			reserved:        defaultReserved,
			wantCapacity:    v1.ResourceList{"cpu": "4000m", "memory": "8Gi"},
			wantAllocatable: v1.ResourceList{"cpu": "3500m", "memory": "7680Mi"},
		},
		{
			name:            "no reservation",
			cpuCores:        2,
			memoryBytes:     4 << 30,
			wantCapacity:    v1.ResourceList{"cpu": "2000m", "memory": "4Gi"},
			wantAllocatable: v1.ResourceList{"cpu": "2000m", "memory": "4Gi"},
		},
		{
			name:            "reservation equals capacity",
			cpuCores:        1,
			memoryBytes:     1 << 30,
			reserved:        SystemReserved{CPUMilli: 1000, MemoryBytes: 1 << 30},
			wantCapacity:    v1.ResourceList{"cpu": "1000m", "memory": "1Gi"},
			wantAllocatable: v1.ResourceList{"cpu": "0m", "memory": "0"},
		},
		{
			name:            "reservation exceeds capacity floors at zero",
			cpuCores:        0,
			memoryBytes:     256 << 20,
			reserved:        defaultReserved,
			wantCapacity:    v1.ResourceList{"cpu": "0m", "memory": "256Mi"},
			wantAllocatable: v1.ResourceList{"cpu": "0m", "memory": "0"},
		},
		{
			name:            "memory not MiB-aligned",
			cpuCores:        8,
			memoryBytes:     8039428 << 10,
			reserved:        defaultReserved,
			wantCapacity:    v1.ResourceList{"cpu": "8000m", "memory": "8039428Ki"},
			wantAllocatable: v1.ResourceList{"cpu": "7500m", "memory": "7515140Ki"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capacity, allocatable := nodeCapacity(tt.cpuCores, tt.memoryBytes, tt.reserved)
			assert.Equal(t, tt.wantCapacity, capacity)
			assert.Equal(t, tt.wantAllocatable, allocatable)
		})
	}
}

func TestProbeNodeCapacity(t *testing.T) {
	reserved := SystemReserved{CPUMilli: 500, MemoryBytes: 512 << 20}

	tests := []struct {
		name            string
		prober          ResourceProber
		wantCapacity    v1.ResourceList
		wantAllocatable v1.ResourceList
	}{
		{
			name:            "measured",
			prober:          fakeProber{cpuCores: 2, memoryBytes: 4 << 30},
			wantCapacity:    v1.ResourceList{"cpu": "2000m", "memory": "4Gi"},
			wantAllocatable: v1.ResourceList{"cpu": "1500m", "memory": "3584Mi"},
		},
		{
			name:   "probe fails",
			prober: fakeProber{err: errors.New("docker daemon unreachable")},
		},
		{
			name: "no prober",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capacity, allocatable := probeNodeCapacity(context.Background(), tt.prober, reserved, zap.NewNop())
			assert.Equal(t, tt.wantCapacity, capacity)
			assert.Equal(t, tt.wantAllocatable, allocatable)
		})
	}
}
