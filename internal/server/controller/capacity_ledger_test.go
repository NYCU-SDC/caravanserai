package controller

import (
	"context"
	"errors"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeCapacityProjectStore serves ProjectSnapshots by node and records the
// phases the ledger asked for.
type fakeCapacityProjectStore struct {
	byNode map[string][]*ProjectSnapshot
	err    error

	gotPhases []v1.ProjectPhase
}

var _ CapacityProjectStore = (*fakeCapacityProjectStore)(nil)

func (f *fakeCapacityProjectStore) ListProjectsByNodeRef(_ context.Context, node string, phases []v1.ProjectPhase) ([]*ProjectSnapshot, error) {
	f.gotPhases = phases
	if f.err != nil {
		return nil, f.err
	}
	return f.byNode[node], nil
}

func snapshots(sizes ...v1.ProjectSize) []*ProjectSnapshot {
	out := make([]*ProjectSnapshot, len(sizes))
	for i, s := range sizes {
		out[i] = &ProjectSnapshot{Name: "p", Size: s}
	}
	return out
}

func newTestLedger(byNode map[string][]*ProjectSnapshot) (*CapacityLedger, *fakeCapacityProjectStore) {
	st := &fakeCapacityProjectStore{byNode: byNode}
	return NewCapacityLedger(zap.NewNop(), st), st
}

func TestCapacityLedgerUsed(t *testing.T) {
	tests := []struct {
		name  string
		sizes []v1.ProjectSize
		want  NodeUsage
	}{
		{name: "empty node", want: NodeUsage{}},
		{
			name:  "mixed sizes are summed",
			sizes: []v1.ProjectSize{v1.ProjectSizeSmall, v1.ProjectSizeMedium, v1.ProjectSizeLarge},
			want:  NodeUsage{CPUMilli: 500 + 1000 + 2000, MemoryBytes: 512<<20 + 2<<30 + 4<<30},
		},
		{
			name:  "omitted size books the default",
			sizes: []v1.ProjectSize{""},
			want:  NodeUsage{CPUMilli: 1000, MemoryBytes: 2 << 30},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger, _ := newTestLedger(map[string][]*ProjectSnapshot{"n1": snapshots(tt.sizes...)})
			got, err := ledger.Used(context.Background(), "n1")
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	t.Run("only phases that hold resources are asked for", func(t *testing.T) {
		ledger, st := newTestLedger(nil)
		_, err := ledger.Used(context.Background(), "n1")
		require.NoError(t, err)
		assert.ElementsMatch(t,
			[]v1.ProjectPhase{v1.ProjectPhaseScheduled, v1.ProjectPhaseRunning, v1.ProjectPhaseTerminating},
			st.gotPhases, "Failed and Pending must not be booked")
	})

	t.Run("other nodes are not counted", func(t *testing.T) {
		ledger, _ := newTestLedger(map[string][]*ProjectSnapshot{
			"n1": snapshots(v1.ProjectSizeSmall),
			"n2": snapshots(v1.ProjectSizeLarge),
		})
		got, err := ledger.Used(context.Background(), "n1")
		require.NoError(t, err)
		assert.Equal(t, int64(500), got.CPUMilli)
	})

	t.Run("store error is returned", func(t *testing.T) {
		ledger, st := newTestLedger(nil)
		st.err = errors.New("db down")
		_, err := ledger.Used(context.Background(), "n1")
		assert.ErrorContains(t, err, "db down")
	})
}

func TestCapacityLedgerFits(t *testing.T) {
	// 4 cores / 8Gi allocatable; one Medium (1c/2Gi) already placed.
	node := func(alloc v1.ResourceList) ReadyNode { return ReadyNode{Name: "n1", Allocatable: alloc} }
	full := v1.ResourceList{"cpu": "4000m", "memory": "8Gi"}

	tests := []struct {
		name       string
		alloc      v1.ResourceList
		placed     []v1.ProjectSize
		size       v1.ProjectSize
		wantFit    bool
		wantReason string
	}{
		{name: "fits with room to spare", alloc: full, placed: []v1.ProjectSize{v1.ProjectSizeMedium}, size: v1.ProjectSizeSmall, wantFit: true},
		{
			// 1c + 2c = 3c of 3c; 2Gi + 4Gi = 6Gi of 6Gi.
			name:    "exact boundary fits",
			alloc:   v1.ResourceList{"cpu": "3", "memory": "6Gi"},
			placed:  []v1.ProjectSize{v1.ProjectSizeMedium},
			size:    v1.ProjectSizeLarge,
			wantFit: true,
		},
		{
			name:       "one millicore over rejects",
			alloc:      v1.ResourceList{"cpu": "2999m", "memory": "6Gi"},
			placed:     []v1.ProjectSize{v1.ProjectSizeMedium},
			size:       v1.ProjectSizeLarge,
			wantReason: "lacks cpu",
		},
		{
			name:       "one byte over rejects",
			alloc:      v1.ResourceList{"cpu": "3", "memory": "6442450943"},
			placed:     []v1.ProjectSize{v1.ProjectSizeMedium},
			size:       v1.ProjectSizeLarge,
			wantReason: "lacks memory",
		},
		{
			name:       "cpu fits but memory does not",
			alloc:      v1.ResourceList{"cpu": "8", "memory": "2Gi"},
			placed:     []v1.ProjectSize{v1.ProjectSizeMedium},
			size:       v1.ProjectSizeSmall,
			wantReason: "lacks memory",
		},
		{
			name:       "full node rejects",
			alloc:      full,
			placed:     []v1.ProjectSize{v1.ProjectSizeLarge, v1.ProjectSizeLarge},
			size:       v1.ProjectSizeSmall,
			wantReason: "lacks cpu",
		},
		{name: "omitted size is checked as the default", alloc: v1.ResourceList{"cpu": "999m", "memory": "8Gi"}, size: "", wantReason: "lacks cpu"},
		{name: "no allocatable reported", alloc: nil, size: v1.ProjectSizeSmall, wantReason: "upgrade cara-agent"},
		{name: "memory not reported", alloc: v1.ResourceList{"cpu": "4"}, size: v1.ProjectSizeSmall, wantReason: "[memory]"},
		{name: "cpu not reported", alloc: v1.ResourceList{"memory": "8Gi"}, size: v1.ProjectSizeSmall, wantReason: "[cpu]"},
		{name: "unparseable cpu", alloc: v1.ResourceList{"cpu": "lots", "memory": "8Gi"}, size: v1.ProjectSizeSmall, wantReason: "unparseable allocatable cpu"},
		{name: "unparseable memory", alloc: v1.ResourceList{"cpu": "4", "memory": "plenty"}, size: v1.ProjectSizeSmall, wantReason: "unparseable allocatable memory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ledger, _ := newTestLedger(map[string][]*ProjectSnapshot{"n1": snapshots(tt.placed...)})
			ok, reason, err := ledger.Fits(context.Background(), node(tt.alloc), tt.size)
			require.NoError(t, err)
			assert.Equal(t, tt.wantFit, ok)
			if tt.wantFit {
				assert.Empty(t, reason)
				return
			}
			assert.Contains(t, reason, tt.wantReason)
			assert.Contains(t, reason, `"n1"`, "reason names the node")
		})
	}

	t.Run("store error is returned, not turned into a verdict", func(t *testing.T) {
		ledger, st := newTestLedger(nil)
		st.err = errors.New("db down")
		ok, _, err := ledger.Fits(context.Background(), node(full), v1.ProjectSizeSmall)
		assert.ErrorContains(t, err, "db down")
		assert.False(t, ok)
	})

	t.Run("a node that has not reported is rejected without reading the store", func(t *testing.T) {
		ledger, st := newTestLedger(nil)
		st.err = errors.New("must not be called")
		ok, reason, err := ledger.Fits(context.Background(), node(nil), v1.ProjectSizeSmall)
		require.NoError(t, err)
		assert.False(t, ok)
		assert.Contains(t, reason, "upgrade cara-agent")
	})
}
