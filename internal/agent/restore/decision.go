package restore

import (
	"fmt"
	"os"
	"path/filepath"
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

	// VolumesHaveData reports whether any Managed volume directory holds
	// content.
	VolumesHaveData bool
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
//   - Leftover staging means the previous restore died before its cleanup ran,
//     so it may have swapped some volumes and not others. Nothing on disk can
//     be trusted to represent a whole generation. Restore again. This mirrors
//     how backup.CleanStaging treats surviving staging as proof of a dead
//     process; the difference is that backup only reclaims the space, whereas
//     here the same signal also invalidates what is on disk.
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
// history distinguishes the one case where having nothing is not a problem: a
// Project that has provably never been assigned anywhere has no data to
// recover, so starting empty is correct rather than a guess.
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
		// Proven ours. One contradiction is still possible: the marker says it
		// restored a generation, and the directories it describes are gone.
		if state.Marker.InitializedFromBackupID != "" && !state.VolumesHaveData {
			return Outcome{
				Decision: DecisionBlock,
				Reason:   BlockMissingData,
				Detail: fmt.Sprintf("provenance records a restore from generation %q but the volumes are empty",
					state.Marker.InitializedFromBackupID),
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

	case state.VolumesHaveData:
		return Outcome{
			Decision: DecisionBlock,
			Reason:   BlockUnprovenData,
			Detail:   "the volumes hold data that no provenance marker accounts for",
		}

	case history == v1.AssignmentHistoryNeverAssigned:
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

// VolumesHaveData reports whether any of the Project's Managed volume
// directories currently holds content.
//
// A volume directory that exists but is empty does not count: the agent
// provisions empty directories for Managed volumes (CARA-66), so mere
// existence proves nothing about whether this node has ever served the
// Project.
func VolumesHaveData(dataRoot, namespace, project string, volumes []v1.VolumeDef) (bool, error) {
	for _, vol := range volumes {
		if vol.Type != v1.VolumeTypeManaged {
			continue
		}

		path, err := caravolume.HostPath(dataRoot, namespace, project, vol.Name)
		if err != nil {
			return false, err
		}

		entries, err := os.ReadDir(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return false, fmt.Errorf("restore: inspect volume dir %q: %w", path, err)
		}
		if len(entries) > 0 {
			return true, nil
		}
	}
	return false, nil
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
