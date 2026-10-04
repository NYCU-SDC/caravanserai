package controller

import (
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// cand builds a Filter candidate on a node of the given Allocatable cores and
// Gi of memory, with the given booked cpu (millicores) and memory (Gi).
func cand(name, tier string, cores, memGi, usedMilli, usedGi int64) candidate {
	var labels map[string]string
	if tier != "" {
		labels = map[string]string{v1.LabelNodeTier: tier}
	}
	return candidate{
		ReadyNode:   ReadyNode{Name: name, Labels: labels},
		Used:        NodeUsage{CPUMilli: usedMilli, MemoryBytes: usedGi << 30},
		Allocatable: NodeUsage{CPUMilli: cores * 1000, MemoryBytes: memGi << 30},
	}
}

func TestLeastAllocated(t *testing.T) {
	// Medium books 1c / 2Gi.
	tests := []struct {
		name string
		c    candidate
		want float64
	}{
		{name: "empty node keeps most of it free", c: cand("n", "", 4, 8, 0, 0), want: 75},
		{name: "half full", c: cand("n", "", 4, 8, 1000, 2), want: 50},
		{name: "filled exactly leaves nothing", c: cand("n", "", 1, 2, 0, 0), want: 0},
		{name: "cpu and memory are averaged", c: cand("n", "", 4, 4, 0, 0), want: (75.0 + 50.0) / 2},
		{name: "zero allocatable scores zero", c: cand("n", "", 0, 0, 0, 0), want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.InDelta(t, tt.want, leastAllocated(tt.c, v1.ProjectSizeMedium), 1e-9)
		})
	}
}

func TestPickBest(t *testing.T) {
	tests := []struct {
		name  string
		cands []candidate
		want  string
	}{
		{
			name:  "primary beats an emptier backup",
			cands: []candidate{cand("backup-empty", "backup", 8, 16, 0, 0), cand("primary-busy", "primary", 8, 16, 4000, 8)},
			want:  "primary-busy",
		},
		{
			name: "primary stays preferred when it is nearly full",
			// Medium leaves the primary node with 1% cpu and 1% memory free.
			cands: []candidate{cand("backup-empty", "backup", 64, 256, 0, 0), cand("primary-tight", "primary", 100, 200, 98000, 196)},
			want:  "primary-tight",
		},
		{
			name:  "emptiest primary wins among primaries",
			cands: []candidate{cand("p1", "primary", 4, 8, 2000, 4), cand("p2", "primary", 4, 8, 0, 0)},
			want:  "p2",
		},
		{
			name:  "emptiest backup wins when no primary is a candidate",
			cands: []candidate{cand("b1", "backup", 4, 8, 2000, 4), cand("b2", "backup", 4, 8, 0, 0)},
			want:  "b2",
		},
		{
			name:  "unlabeled node counts as backup",
			cands: []candidate{cand("plain", "", 8, 16, 0, 0), cand("p", "primary", 8, 16, 4000, 8)},
			want:  "p",
		},
		{
			name:  "unlabeled node competes with backup on headroom alone",
			cands: []candidate{cand("plain", "", 4, 8, 0, 0), cand("b", "backup", 4, 8, 2000, 4)},
			want:  "plain",
		},
		{
			name:  "equal scores go to the name that sorts first",
			cands: []candidate{cand("node-b", "primary", 4, 8, 0, 0), cand("node-a", "primary", 4, 8, 0, 0)},
			want:  "node-a",
		},
		{
			name:  "equal scores resolve the same way in the other order",
			cands: []candidate{cand("node-a", "primary", 4, 8, 0, 0), cand("node-b", "primary", 4, 8, 0, 0)},
			want:  "node-a",
		},
		{
			name:  "a single candidate is chosen",
			cands: []candidate{cand("only", "backup", 2, 4, 0, 0)},
			want:  "only",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := pickBest(tt.cands, v1.ProjectSizeMedium)
			assert.Equal(t, tt.want, got.Name)
		})
	}
}

func TestScoreCandidate_PrimaryBonusIsStrict(t *testing.T) {
	// The emptiest possible backup node against the fullest primary node that
	// still passed Filter: the bonus must still decide it.
	emptiestBackup := scoreCandidate(cand("b", "backup", 1000, 1000, 0, 0), v1.ProjectSizeSmall)
	fullestPrimary := scoreCandidate(cand("p", "primary", 1, 2, 0, 0), v1.ProjectSizeLarge)
	assert.Greater(t, fullestPrimary, emptiestBackup)
	assert.Greater(t, TierPrimaryBonus, 100.0, "a bonus above the 0..100 range is what makes the preference strict")
}

func TestProjectSchedulerScore(t *testing.T) {
	tier := func(name, t string) ReadyNode {
		return ReadyNode{Name: name, Labels: map[string]string{v1.LabelNodeTier: t}, Allocatable: v1.ResourceList{"cpu": "4", "memory": "8Gi"}}
	}
	run := func(t *testing.T, ps *fakeSchedulerProjectStore, nodes ...ReadyNode) *fakeSchedulerProjectStore {
		t.Helper()
		ps.projects["my-app"] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, Size: v1.ProjectSizeMedium}
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, newFakeSchedulerNodeStoreOf(nodes...), nil)
		_, err := ctrl.Reconcile(t.Context(), "my-app")
		require.NoError(t, err)
		return ps
	}
	placedOn := func(t *testing.T, ps *fakeSchedulerProjectStore) string {
		t.Helper()
		require.Len(t, ps.SetProjectScheduledCalls, 1)
		return ps.SetProjectScheduledCalls[0].NodeRef
	}

	t.Run("with room on both tiers the project goes to primary", func(t *testing.T) {
		ps := run(t, newFakeSchedulerProjectStore(), tier("a-backup", "backup"), tier("z-primary", "primary"))
		assert.Equal(t, "z-primary", placedOn(t, ps), "listed second, and its name sorts last, but it is primary")
	})

	t.Run("when primary is full the project falls back to backup", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		// Two Large projects (2c / 4Gi each) fill the 4c / 8Gi primary node.
		for _, n := range []string{"l1", "l2"} {
			ps.projects[n] = schedulerProjectRecord{Phase: v1.ProjectPhaseRunning, NodeRef: "pve1", Size: v1.ProjectSizeLarge}
		}
		ps = run(t, ps, tier("pve1", "primary"), tier("twcc1", "backup"))
		assert.Equal(t, "twcc1", placedOn(t, ps))
	})

	t.Run("when primary is under pressure the project falls back to backup", func(t *testing.T) {
		pressured := tier("pve1", "primary")
		pressured.Conditions = []v1.Condition{{Type: v1.ConditionTypeDiskPressure, Status: v1.ConditionTrue}}
		ps := run(t, newFakeSchedulerProjectStore(), pressured, tier("twcc1", "backup"))
		assert.Equal(t, "twcc1", placedOn(t, ps))
	})

	t.Run("among primary nodes the one with most headroom is chosen", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		ps.projects["busy"] = schedulerProjectRecord{Phase: v1.ProjectPhaseRunning, NodeRef: "pve1", Size: v1.ProjectSizeLarge}
		ps = run(t, ps, tier("pve1", "primary"), tier("pve2", "primary"))
		assert.Equal(t, "pve2", placedOn(t, ps))
	})

	t.Run("projects spread across equal nodes as they are placed", func(t *testing.T) {
		ps := newFakeSchedulerProjectStore()
		for _, n := range []string{"a", "b"} {
			ps.projects[n] = schedulerProjectRecord{Phase: v1.ProjectPhasePending, Size: v1.ProjectSizeMedium}
		}
		ns := newFakeSchedulerNodeStoreOf(tier("pve1", "primary"), tier("pve2", "primary"))
		ctrl := NewProjectSchedulerController(zap.NewNop(), ps, ns, nil)
		for _, n := range []string{"a", "b"} {
			_, err := ctrl.Reconcile(t.Context(), n)
			require.NoError(t, err)
		}
		require.Len(t, ps.SetProjectScheduledCalls, 2)
		assert.NotEqual(t, ps.SetProjectScheduledCalls[0].NodeRef, ps.SetProjectScheduledCalls[1].NodeRef,
			"the second project goes to the node the first one did not fill")
	})
}
