package controller

import (
	"context"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func cond(t v1.ConditionType, s v1.ConditionStatus) v1.Condition {
	return v1.Condition{Type: t, Status: s}
}

func TestFilterNodes(t *testing.T) {
	alloc := v1.ResourceList{"cpu": "4", "memory": "8Gi"}
	tests := []struct {
		name       string
		nodes      []ReadyNode
		wantNames  []string
		wantReject string // substring expected in the rejection for the node left out
	}{
		{
			name:      "node with room is a candidate",
			nodes:     []ReadyNode{{Name: "n1", Allocatable: alloc}},
			wantNames: []string{"n1"},
		},
		{
			name:      "pressure conditions that are False are ignored",
			nodes:     []ReadyNode{{Name: "n1", Allocatable: alloc, Conditions: []v1.Condition{cond(v1.ConditionTypeMemoryPressure, v1.ConditionFalse), cond(v1.ConditionTypeDiskPressure, v1.ConditionFalse)}}},
			wantNames: []string{"n1"},
		},
		{
			name:      "pressure conditions that are Unknown are ignored",
			nodes:     []ReadyNode{{Name: "n1", Allocatable: alloc, Conditions: []v1.Condition{cond(v1.ConditionTypeMemoryPressure, v1.ConditionUnknown)}}},
			wantNames: []string{"n1"},
		},
		{
			name:      "unrelated True condition is ignored",
			nodes:     []ReadyNode{{Name: "n1", Allocatable: alloc, Conditions: []v1.Condition{cond("Other", v1.ConditionTrue)}}},
			wantNames: []string{"n1"},
		},
		{
			name:       "MemoryPressure excludes",
			nodes:      []ReadyNode{{Name: "n1", Allocatable: alloc, Conditions: []v1.Condition{cond(v1.ConditionTypeMemoryPressure, v1.ConditionTrue)}}},
			wantReject: "MemoryPressure",
		},
		{
			name:       "DiskPressure excludes",
			nodes:      []ReadyNode{{Name: "n1", Allocatable: alloc, Conditions: []v1.Condition{cond(v1.ConditionTypeDiskPressure, v1.ConditionTrue)}}},
			wantReject: "DiskPressure",
		},
		{
			name:       "node without allocatable is excluded",
			nodes:      []ReadyNode{{Name: "n1"}},
			wantReject: "upgrade cara-agent",
		},
		{
			name:       "node too small for the project is excluded",
			nodes:      []ReadyNode{{Name: "n1", Allocatable: v1.ResourceList{"cpu": "500m", "memory": "8Gi"}}},
			wantReject: "lacks cpu",
		},
		{
			name: "only the nodes that pass are kept, in order",
			nodes: []ReadyNode{
				{Name: "pressured", Allocatable: alloc, Conditions: []v1.Condition{cond(v1.ConditionTypeDiskPressure, v1.ConditionTrue)}},
				{Name: "ok-1", Allocatable: alloc},
				{Name: "tiny", Allocatable: v1.ResourceList{"cpu": "100m", "memory": "8Gi"}},
				{Name: "ok-2", Allocatable: alloc},
			},
			wantNames: []string{"ok-1", "ok-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger, _ := newTestLedger(nil)
			got, rejected, err := filterNodes(context.Background(), ledger, tt.nodes, v1.ProjectSizeMedium)
			require.NoError(t, err)

			var names []string
			for _, n := range got {
				names = append(names, n.Name)
			}
			assert.Equal(t, tt.wantNames, names)

			if tt.wantReject != "" {
				require.Len(t, rejected, 1)
				assert.Contains(t, rejected[0], tt.wantReject)
				assert.Contains(t, rejected[0], `"n1"`, "rejection names the node")
			}
		})
	}

	t.Run("capacity already booked on the node counts", func(t *testing.T) {
		// 2c of 4c booked by a Large project; a second Large fits exactly,
		// a third would not.
		ledger, _ := newTestLedger(map[string][]*ProjectSnapshot{"n1": snapshots(v1.ProjectSizeLarge)})
		n := ReadyNode{Name: "n1", Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}}

		got, _, err := filterNodes(context.Background(), ledger, []ReadyNode{n}, v1.ProjectSizeLarge)
		require.NoError(t, err)
		assert.Len(t, got, 1, "a second Large fits exactly")

		ledger, _ = newTestLedger(map[string][]*ProjectSnapshot{"n1": snapshots(v1.ProjectSizeLarge, v1.ProjectSizeLarge)})
		got, rejected, err := filterNodes(context.Background(), ledger, []ReadyNode{n}, v1.ProjectSizeSmall)
		require.NoError(t, err)
		assert.Empty(t, got, "a full node has no room even for Small")
		require.Len(t, rejected, 1)
	})

	t.Run("ledger error is returned, not treated as full", func(t *testing.T) {
		ledger := NewCapacityLedger(zap.NewNop(), &fakeCapacityProjectStore{err: assert.AnError})
		got, _, err := filterNodes(context.Background(), ledger,
			[]ReadyNode{{Name: "n1", Allocatable: alloc}}, v1.ProjectSizeSmall)
		assert.ErrorIs(t, err, assert.AnError)
		assert.Empty(t, got)
	})
}
