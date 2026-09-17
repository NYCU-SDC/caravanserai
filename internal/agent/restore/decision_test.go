package restore

import (
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func currentAssignment() Provenance {
	return Provenance{
		Namespace: "default", Project: "blog",
		ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 7,
	}
}

// markerFor builds an on-disk marker for an assignment, as WriteMarker would.
func markerFor(prov Provenance) *Marker {
	return &Marker{
		Version:                 MarkerVersion,
		Namespace:               prov.Namespace,
		Project:                 prov.Project,
		ProjectUID:              prov.ProjectUID,
		NodeName:                prov.NodeName,
		AssignmentGeneration:    prov.AssignmentGeneration,
		InitializedFromBackupID: prov.BackupID,
		EstablishedAt:           time.Now().UTC(),
	}
}

func TestDecide(t *testing.T) {
	want := currentAssignment()

	mine := func(backupID string) *Marker {
		p := want
		p.BackupID = backupID
		return markerFor(p)
	}
	otherGeneration := func() *Marker {
		p := want
		p.AssignmentGeneration = 5
		return markerFor(p)
	}
	otherLifetime := func() *Marker {
		p := want
		p.ProjectUID = "uid-0"
		return markerFor(p)
	}
	otherNode := func() *Marker {
		p := want
		p.NodeName = "node-b"
		return markerFor(p)
	}
	legacy := func() *Marker {
		return &Marker{Version: 1, Namespace: "default", Project: "blog"}
	}

	tests := []struct {
		name     string
		state    PlacementState
		history  v1.AssignmentHistory
		decision Decision
		reason   BlockReason
	}{
		{
			name:     "leftover staging invalidates whatever is on disk",
			state:    PlacementState{StagingPresent: true, Marker: mine("gen-1"), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionRestore,
		},
		{
			name:     "provenance for this exact assignment reuses local data",
			state:    PlacementState{Marker: mine("gen-1"), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionSkip,
		},
		{
			name:     "initialised empty with nothing written yet is still ours",
			state:    PlacementState{Marker: mine(""), VolumesHaveData: false},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionSkip,
		},
		{
			name:     "restored from a generation but the volumes are gone",
			state:    PlacementState{Marker: mine("gen-1"), VolumesHaveData: false},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockMissingData,
		},
		{
			name:     "an earlier generation on this node predates a move away",
			state:    PlacementState{Marker: otherGeneration(), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "another lifetime of the same name",
			state:    PlacementState{Marker: otherLifetime(), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "a copy that names another node",
			state:    PlacementState{Marker: otherNode(), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "a marker from before provenance existed",
			state:    PlacementState{Marker: legacy(), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockLegacyProvenance,
		},
		{
			name:     "data nothing accounts for is not adopted",
			state:    PlacementState{VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockUnprovenData,
		},
		{
			name:     "an unreadable marker is not an absent one",
			state:    PlacementState{MarkerErr: errors.New("invalid character"), VolumesHaveData: true},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockCorruptProvenance,
		},
		{
			name:     "a Project that has never been placed starts empty",
			state:    PlacementState{},
			history:  v1.AssignmentHistoryNeverAssigned,
			decision: DecisionInitializeEmpty,
		},
		{
			name:     "a clean node for a Project that has run before restores",
			state:    PlacementState{},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionRestore,
		},
		{
			name:     "unknown history on a clean node restores rather than starting empty",
			state:    PlacementState{},
			history:  v1.AssignmentHistoryUnknown,
			decision: DecisionRestore,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.state, want, tt.history)
			assert.Equal(t, tt.decision, got.Decision)
			assert.Equal(t, tt.reason, got.Reason)
			if tt.decision == DecisionBlock {
				assert.NotEmpty(t, got.Detail, "a block must say why")
			}
		})
	}
}

// The defect this package was changed for: a directory left behind by an
// earlier assignment used to be proof enough to skip the restore, so the
// Project started on data that belonged to a Node it had since moved away
// from. Whatever else changes, this must never resolve to Skip again.
func TestDecideNeverSkipsOnForeignProvenance(t *testing.T) {
	want := currentAssignment()

	foreign := []struct {
		name   string
		marker *Marker
	}{
		{"earlier generation", func() *Marker { p := want; p.AssignmentGeneration = 6; return markerFor(p) }()},
		{"later generation", func() *Marker { p := want; p.AssignmentGeneration = 8; return markerFor(p) }()},
		{"another lifetime", func() *Marker { p := want; p.ProjectUID = "uid-0"; return markerFor(p) }()},
		{"another node", func() *Marker { p := want; p.NodeName = "node-z"; return markerFor(p) }()},
		{"legacy schema", &Marker{Version: 1}},
	}

	for _, f := range foreign {
		t.Run(f.name, func(t *testing.T) {
			for _, hasData := range []bool{true, false} {
				got := Decide(PlacementState{Marker: f.marker, VolumesHaveData: hasData}, want,
					v1.AssignmentHistoryKnown)
				require.Equal(t, DecisionBlock, got.Decision, "volumesHaveData=%v", hasData)
			}
		})
	}
}

// Detail reaches an operator through Project status, so it may name the
// Project's own identity and must not leak this node's filesystem layout.
func TestBlockDetailNamesIdentityNotPaths(t *testing.T) {
	want := currentAssignment()
	other := want
	other.NodeName = "node-b"

	got := Decide(PlacementState{Marker: markerFor(other), VolumesHaveData: true}, want,
		v1.AssignmentHistoryKnown)

	require.Equal(t, DecisionBlock, got.Decision)
	assert.Contains(t, got.Detail, "node-b")
	assert.NotContains(t, got.Detail, "/var/lib")
	assert.NotContains(t, got.Detail, dataDirForTest)
}

const dataDirForTest = "/volumes/"
