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

		// firstGen places the assignment at generation 1, which is what an
		// Agent sees on a Project's very first placement.
		firstGen bool
	}{
		{
			name:     "leftover staging invalidates whatever is on disk",
			state:    PlacementState{StagingPresent: true, Marker: mine("gen-1"), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionRestore,
		},
		{
			name:     "provenance for this exact assignment reuses local data",
			state:    PlacementState{Marker: mine("gen-1"), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionSkip,
		},
		{
			name:     "initialised empty with nothing written yet is still ours",
			state:    PlacementState{Marker: mine(""), Volumes: VolumeSurvey{Empty: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionSkip,
		},
		{
			name:     "restored from a generation but the volumes are gone",
			state:    PlacementState{Marker: mine("gen-1"), Volumes: VolumeSurvey{Empty: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockMissingData,
		},
		{
			name:     "an earlier generation on this node predates a move away",
			state:    PlacementState{Marker: otherGeneration(), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "another lifetime of the same name",
			state:    PlacementState{Marker: otherLifetime(), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "a copy that names another node",
			state:    PlacementState{Marker: otherNode(), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockForeignProvenance,
		},
		{
			name:     "a marker from before provenance existed",
			state:    PlacementState{Marker: legacy(), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockLegacyProvenance,
		},
		{
			name:     "data nothing accounts for is not adopted",
			state:    PlacementState{Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockUnprovenData,
		},
		{
			name:     "an unreadable marker is not an absent one",
			state:    PlacementState{MarkerErr: errors.New("invalid character"), Volumes: VolumeSurvey{WithData: []string{"db-data"}}},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionBlock,
			reason:   BlockCorruptProvenance,
		},
		{
			// The realistic first-placement state: the server grants the first
			// assignment and marks history Known in the same write, so this is
			// what an Agent actually sees for a brand-new Project.
			name:     "the first grant of ownership starts empty",
			state:    PlacementState{},
			history:  v1.AssignmentHistoryKnown,
			decision: DecisionInitializeEmpty,
			firstGen: true,
		},
		{
			name:     "unprovable history never starts empty, even at generation 1",
			state:    PlacementState{},
			history:  v1.AssignmentHistoryUnknown,
			decision: DecisionRestore,
			firstGen: true,
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
			assignment := want
			if tt.firstGen {
				assignment.AssignmentGeneration = 1
			}
			got := Decide(tt.state, assignment, tt.history)
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
			surveys := map[string]VolumeSurvey{
				"with data": {WithData: []string{"db-data"}},
				"empty":     {Empty: []string{"db-data"}},
			}
			for name, survey := range surveys {
				got := Decide(PlacementState{Marker: f.marker, Volumes: survey}, want,
					v1.AssignmentHistoryKnown)
				require.Equal(t, DecisionBlock, got.Decision, "volumes %s", name)
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

	got := Decide(PlacementState{Marker: markerFor(other), Volumes: VolumeSurvey{WithData: []string{"db-data"}}}, want,
		v1.AssignmentHistoryKnown)

	require.Equal(t, DecisionBlock, got.Decision)
	assert.Contains(t, got.Detail, "node-b")
	assert.NotContains(t, got.Detail, "/var/lib")
	assert.NotContains(t, got.Detail, dataDirForTest)
}

const dataDirForTest = "/volumes/"

// A Project with several Managed volumes is complete or it is not. One healthy
// volume used to be enough to answer "does this Project have its data", so a
// Project whose uploads directory had been deleted skipped the restore and had
// it recreated empty under a service that believed the files were there.
func TestDecideBlocksWhenOnlySomeVolumesSurvive(t *testing.T) {
	want := currentAssignment()
	restored := want
	restored.BackupID = "gen-1"

	got := Decide(PlacementState{
		Marker:  markerFor(restored),
		Volumes: VolumeSurvey{WithData: []string{"db-data"}, Empty: []string{"uploads"}},
	}, want, v1.AssignmentHistoryKnown)

	require.Equal(t, DecisionBlock, got.Decision)
	assert.Equal(t, BlockMissingData, got.Reason)
	assert.Contains(t, got.Detail, "uploads", "the operator has to be told which volume is gone")
	assert.NotContains(t, got.Detail, "db-data", "naming the healthy volume would mislead")
}

// The mirror of the case above: every volume intact is the ordinary path and
// must stay cheap.
func TestDecideSkipsWhenEveryVolumeSurvives(t *testing.T) {
	want := currentAssignment()
	restored := want
	restored.BackupID = "gen-1"

	got := Decide(PlacementState{
		Marker:  markerFor(restored),
		Volumes: VolumeSurvey{WithData: []string{"db-data", "uploads"}},
	}, want, v1.AssignmentHistoryKnown)

	assert.Equal(t, DecisionSkip, got.Decision)
}

// A Project initialised empty legitimately has empty volumes, so emptiness
// alone is not a contradiction — only emptiness against a marker that claims a
// generation was restored.
func TestDecideAllowsEmptyVolumesWhenNoGenerationWasRestored(t *testing.T) {
	want := currentAssignment()

	got := Decide(PlacementState{
		Marker:  markerFor(want), // BackupID empty: initialised, never restored
		Volumes: VolumeSurvey{Empty: []string{"db-data", "uploads"}},
	}, want, v1.AssignmentHistoryKnown)

	assert.Equal(t, DecisionSkip, got.Decision)
}

// AssignmentHistory cannot be the signal for "first placement". The server
// sets it to Known in the same write that grants the first assignment, so an
// Agent never observes NeverAssigned for a Project it is being asked to place.
// Deciding on it refused every new Project with a Managed volume — a
// regression the unit tests missed because their fixtures hand-built a state
// the system does not produce.
func TestDecideStartsEmptyOnTheStateAnAgentActuallySees(t *testing.T) {
	firstPlacement := Provenance{
		Namespace: "default", Project: "blog",
		ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 1,
	}

	got := Decide(PlacementState{}, firstPlacement, v1.AssignmentHistoryKnown)
	assert.Equal(t, DecisionInitializeEmpty, got.Decision,
		"a brand-new Project must be placeable")

	// Second placement of the same Project: it ran somewhere, so its data is
	// in the object store or nowhere.
	later := firstPlacement
	later.AssignmentGeneration = 2
	got = Decide(PlacementState{}, later, v1.AssignmentHistoryKnown)
	assert.Equal(t, DecisionRestore, got.Decision)
}

// Generation 0 means no assignment has ever been granted, so an Agent holding
// one has been handed a state the server does not produce. Creating volumes on
// the strength of it would be failing open on an anomaly.
func TestDecideRefusesToStartEmptyAtGenerationZero(t *testing.T) {
	anomalous := Provenance{
		Namespace: "default", Project: "blog",
		ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 0,
	}

	got := Decide(PlacementState{}, anomalous, v1.AssignmentHistoryKnown)

	assert.NotEqual(t, DecisionInitializeEmpty, got.Decision)
	assert.Equal(t, DecisionRestore, got.Decision)
}
