package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
)

// Decision is what the agent should do with a Project's Managed volumes
// before starting its containers.
type Decision int

const (
	// DecisionRestore means the local volumes cannot be used and the newest
	// complete generation must be pulled from the object store.
	DecisionRestore Decision = iota

	// DecisionSkip means provenance proves the local data belongs to this
	// exact assignment. The local volumes are authoritative and must not be
	// touched.
	DecisionSkip

	// DecisionInitializeEmpty means this Project has provably never been
	// placed anywhere, so there is nothing to restore and nothing to lose.
	DecisionInitializeEmpty

	// DecisionBlock means the data source cannot be established safely.
	// Nothing is started, nothing is created, and nothing is overwritten.
	//
	// It replaces what used to be DecisionAdoptExisting. Adopting data whose
	// origin is unknown was the mechanism behind silent loss: a directory left
	// by another lifetime or another assignment was taken as this Project's,
	// stamped with a marker, served to clients, and then backed up over the
	// generation that held the real data.
	DecisionBlock
)

func (d Decision) String() string {
	switch d {
	case DecisionRestore:
		return "Restore"
	case DecisionSkip:
		return "Skip"
	case DecisionInitializeEmpty:
		return "InitializeEmpty"
	case DecisionBlock:
		return "Block"
	default:
		return fmt.Sprintf("Decision(%d)", int(d))
	}
}

// BlockReason names why a placement was refused. It is the machine-readable
// half of the decision: it reaches Project status as a condition reason, so it
// must name a class of problem rather than describe one occurrence.
type BlockReason string

const (
	// BlockNone is the zero value, used when the decision is not Block.
	BlockNone BlockReason = ""

	// BlockLegacyProvenance means a marker exists but predates provenance, so
	// which assignment produced the data cannot be determined.
	BlockLegacyProvenance BlockReason = "LegacyProvenance"

	// BlockForeignProvenance means the marker names a different Project
	// lifetime, Node, or assignment generation.
	BlockForeignProvenance BlockReason = "ForeignProvenance"

	// BlockUnprovenData means volumes hold data with no marker at all.
	BlockUnprovenData BlockReason = "UnprovenData"

	// BlockCorruptProvenance means the marker exists but could not be read.
	BlockCorruptProvenance BlockReason = "CorruptProvenance"

	// BlockNoRestoreSource means the Project has been placed before but the
	// object store holds no complete generation to restore from. It is not
	// the same as a new Project with no backups: that one has nothing to lose
	// and starts empty. This one had data somewhere, and where it went is a
	// question the agent cannot answer.
	BlockNoRestoreSource BlockReason = "NoRestoreSource"

	// BlockMissingData means the marker claims data restored from a
	// generation while the volume directories are absent or empty. The two
	// statements cannot both be true, so neither is trusted.
	BlockMissingData BlockReason = "MissingData"
)

// PlacementState is what the agent found on disk for one Project.
type PlacementState struct {
	// StagingPresent reports leftover restore staging.
	StagingPresent bool

	// Marker is the parsed marker, nil when absent.
	Marker *Marker

	// MarkerErr is set when a marker exists but could not be read. It is kept
	// apart from Marker == nil: absent means this node never established data,
	// unreadable means it may have and we cannot tell.
	MarkerErr error

	// Volumes is the per-volume state of the Project's Managed volumes.
	Volumes VolumeSurvey
}

// Outcome is a decision and, when it is Block, why.
type Outcome struct {
	Decision Decision
	Reason   BlockReason

	// Detail carries the specific mismatch for logs and operator-facing
	// messages. It names fields and values from the Project's own identity,
	// never host paths.
	Detail string
}

// Decide chooses what to do with a Project's Managed volumes before its
// containers start.
//
// The ordering is deliberate and is the core safety property of this package:
//
//   - Leftover staging wins over everything, including a marker that names
//     this exact assignment. This is a deliberate fail-closed policy, not a
//     claim that staging proves the restore was interrupted. Staging survives
//     three different endings, and on disk they are indistinguishable:
//
//     1. The swap completed, the marker was written, and only the cleanup
//     failed. Restoring again costs whatever the containers wrote since.
//     2. The swap failed and rolled back cleanly. The previous marker still
//     describes the data accurately; restoring again is merely redundant.
//     3. The swap failed and the rollback failed too. The volumes are split
//     across two generations and no marker describes them.
//
//     Only the third is dangerous, and it is the one a marker cannot warn
//     about — swapAll writes no marker when it fails, so the marker left on
//     disk is the previous one and it reads as authoritative. Preferring the
//     marker would serve mixed data in that case. Preferring staging costs a
//     redundant restore in the first two. Between losing recent writes and
//     serving a Project data from two different points in time, this package
//     takes the first.
//
//     Making the three distinguishable means giving restore staging a
//     per-generation identity, so "staging for the generation the marker
//     names" can be told from "staging for a generation that never landed".
//     Until that exists, the ambiguity is resolved by refusing to guess.
//
//   - An unreadable marker is not an absent one. Absent means this node never
//     established data; unreadable means it may have and we cannot tell, and
//     the second must not be resolved by guessing.
//
//   - A marker that matches this exact assignment is the only proof local data
//     may be reused. It is checked before anything else about the data,
//     because this is the case that must keep working: containers write
//     continuously and backups run on an interval, so an agent restart that
//     restored from S3 would roll back everything written since the last one.
//
//   - Any other marker — older schema, another lifetime, another Node, an
//     earlier generation — proves the data is not this assignment's. Restoring
//     over it is not safe either, because in this ticket nothing quarantines
//     what is displaced, so the answer is to stop.
//
//   - Data with no marker is the same class of problem arriving by a different
//     route, and used to be adopted silently.
//
// history and the generation in want distinguish the one case where having
// nothing is not a problem: a Project being placed for the very first time has
// never run anywhere, so it has no data to recover and starting empty is
// correct rather than a guess.
//
// The generation is what says "first placement", not AssignmentHistory. The
// server sets history to Known in the same write that grants the first
// assignment, so by the time an Agent is ever asked to place a Project the
// history already reads Known and NeverAssigned is not an observable state.
// Deciding on it would refuse every new Project with a Managed volume.
//
// Generation 1 is the first grant of ownership that has ever existed for this
// Project, so nothing can have run before it. Unknown history overrides that:
// a row whose past could not be established is not one whose counter can be
// trusted either.
func Decide(state PlacementState, want Provenance, history v1.AssignmentHistory) Outcome {
	switch {
	case state.StagingPresent:
		return Outcome{Decision: DecisionRestore}

	case state.MarkerErr != nil:
		return Outcome{
			Decision: DecisionBlock,
			Reason:   BlockCorruptProvenance,
			Detail:   "the local provenance marker could not be read",
		}

	case state.Marker.Matches(want):
		// Proven ours. One contradiction is still possible: the marker says a
		// generation was restored here, and some of what that generation
		// contained is gone.
		//
		// Every declared volume must be intact, not merely one of them. A
		// Project with a healthy database volume and a deleted uploads volume
		// would otherwise skip the restore and have uploads recreated empty
		// under a service that believes its files are there.
		if state.Marker.InitializedFromBackupID != "" && !state.Volumes.Complete() {
			return Outcome{
				Decision: DecisionBlock,
				Reason:   BlockMissingData,
				Detail: fmt.Sprintf("provenance records a restore from generation %q, but %s %s no data",
					state.Marker.InitializedFromBackupID,
					volumeList(state.Volumes.Empty), plural(len(state.Volumes.Empty), "holds", "hold")),
			}
		}
		return Outcome{Decision: DecisionSkip}

	case state.Marker != nil && state.Marker.IsLegacy():
		return Outcome{
			Decision: DecisionBlock,
			Reason:   BlockLegacyProvenance,
			Detail:   "local data predates provenance recording, so the assignment that produced it is unknown",
		}

	case state.Marker != nil:
		return Outcome{
			Decision: DecisionBlock,
			Reason:   BlockForeignProvenance,
			Detail: "local data belongs to another assignment (" +
				strings.Join(state.Marker.Mismatches(want), "; ") + ")",
		}

	case state.Volumes.AnyData():
		return Outcome{
			Decision: DecisionBlock,
			Reason:   BlockUnprovenData,
			Detail:   "the volumes hold data that no provenance marker accounts for",
		}

	case firstPlacement(want, history):
		return Outcome{Decision: DecisionInitializeEmpty}

	default:
		// Nothing local, and this Project has been placed before. Whatever it
		// wrote is in the object store or nowhere.
		return Outcome{Decision: DecisionRestore}
	}
}

// StagingPresent reports whether a restore left staging behind.
//
// Staging is removed only when a restore succeeds — deliberately not on the
// failure path, and never with a defer. A failed restore may have swapped some
// volumes and not others, so leaving staging is what records that the volumes
// may be split across generations. Anything still here therefore means either
// a restore that failed or one whose process died part-way; both are grounds
// to distrust what is on disk. Do not "fix" this by clearing staging on error:
// TestRestoreGenerationKeepsStagingAfterFailure exists to catch that.
func StagingPresent(dataRoot, namespace, project string) (bool, error) {
	dir, err := StagingDir(dataRoot, namespace, project)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("restore: inspect staging %q: %w", dir, err)
	}
	return true, nil
}

// VolumeSurvey is the per-volume state of a Project's Managed volumes.
//
// It is per-volume and not a single boolean because the two facts a decision
// needs are about different volumes: "something here is worth protecting" is
// true if any volume has content, while "the data this marker describes is
// intact" is false if any single one is gone. Collapsing them let a Project
// with one healthy volume and one deleted volume read as complete, skip the
// restore, and have the missing one recreated empty underneath it.
type VolumeSurvey struct {
	// WithData names the Managed volumes that hold content.
	WithData []string

	// Empty names the Managed volumes whose directory is absent or holds
	// nothing. The two are deliberately one category: a Managed volume
	// directory is provisioned empty at create time (CARA-66), so its mere
	// existence proves nothing about whether data was ever written.
	Empty []string
}

// AnyData reports whether any Managed volume holds content.
func (s VolumeSurvey) AnyData() bool { return len(s.WithData) > 0 }

// Complete reports whether every declared Managed volume holds content.
func (s VolumeSurvey) Complete() bool { return len(s.Empty) == 0 }

// SurveyVolumes inspects every Managed volume the Project declares.
func SurveyVolumes(dataRoot, namespace, project string, volumes []v1.VolumeDef) (VolumeSurvey, error) {
	var survey VolumeSurvey

	for _, vol := range volumes {
		if vol.Type != v1.VolumeTypeManaged {
			continue
		}

		path, err := caravolume.HostPath(dataRoot, namespace, project, vol.Name)
		if err != nil {
			return VolumeSurvey{}, err
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			if os.IsNotExist(err) {
				survey.Empty = append(survey.Empty, vol.Name)
				continue
			}
			return VolumeSurvey{}, fmt.Errorf("restore: inspect volume dir %q: %w", path, err)
		}
		if len(entries) == 0 {
			survey.Empty = append(survey.Empty, vol.Name)
			continue
		}
		survey.WithData = append(survey.WithData, vol.Name)
	}
	return survey, nil
}

// StagingDir returns where a restore stages downloaded archives before
// swapping them into place:
//
//	{dataRoot}/restore-staging/{namespace}/{project}
//
// It lives outside the volumes tree so a partially-extracted generation can
// never be mistaken for live volume data, and is on the same filesystem as
// the volumes so the final swap is a rename rather than a copy.
func StagingDir(dataRoot, namespace, project string) (string, error) {
	// Derive through ProjectDir so namespace/project get the same validation
	// and containment checks as every other path in the volumes tree.
	if _, err := caravolume.ProjectDir(dataRoot, namespace, project); err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(dataRoot), stagingRoot, namespace, project), nil
}

// PlacementBlockedError reports that a Project's Managed volume data source
// could not be established safely, so nothing was started.
//
// It is an error rather than a status because every caller must stop: the
// Project has no data it is allowed to use, and the alternatives — starting on
// someone else's data, or creating an empty directory and calling that success
// — are the two failures this package exists to prevent.
type PlacementBlockedError struct {
	Reason BlockReason
	Detail string
}

func (e *PlacementBlockedError) Error() string {
	return fmt.Sprintf("restore: placement blocked (%s): %s", e.Reason, e.Detail)
}

// volumeList renders volume names for an operator-facing message. Names come
// from the Project's own spec, so they are safe to publish; host paths are not
// and never appear here.
func volumeList(names []string) string {
	switch len(names) {
	case 0:
		return "no volume"
	case 1:
		return "volume " + strconv.Quote(names[0])
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = strconv.Quote(n)
	}
	return "volumes " + strings.Join(quoted, ", ")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// firstPlacement reports whether this is the first grant of ownership this
// Project has ever held, and therefore whether "no data anywhere" is its
// correct starting state rather than a fault.
//
// A same-name Project recreated after a delete also arrives at generation 1,
// and a backup may still exist under that name from the lifetime before it.
// Starting empty is right there too: the earlier lifetime's data belongs to a
// different Project UID, and adopting it because the names match is the class
// of mistake this package exists to prevent. Recovering it is a deliberate
// cross-lifetime restore, named by the operator.
func firstPlacement(want Provenance, history v1.AssignmentHistory) bool {
	if history == v1.AssignmentHistoryUnknown {
		return false
	}
	return want.AssignmentGeneration <= 1
}
