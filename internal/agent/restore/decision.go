package restore

import (
	"fmt"
	"os"
	"path/filepath"

	v1 "NYCU-SDC/caravanserai/api/v1"
	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
)

// Decision is what the agent should do with a Project's Managed volumes
// before starting its containers.
type Decision int

const (
	// DecisionRestore means this node has no local data for the Project and
	// should pull the newest complete generation from the object store.
	DecisionRestore Decision = iota

	// DecisionSkip means a marker shows this node has already established
	// local data for the Project. The local volumes are authoritative and must
	// not be touched.
	DecisionSkip

	// DecisionAdoptExisting means the volumes already hold data but no marker
	// records why. The data is kept and a marker is written to record this
	// node's ownership from now on.
	DecisionAdoptExisting
)

func (d Decision) String() string {
	switch d {
	case DecisionRestore:
		return "Restore"
	case DecisionSkip:
		return "Skip"
	case DecisionAdoptExisting:
		return "AdoptExisting"
	default:
		return fmt.Sprintf("Decision(%d)", int(d))
	}
}

// Decide chooses whether to restore, given the three facts that matter.
//
// The ordering is deliberate and is the core safety property of this package:
//
//   - Leftover staging means the previous restore died before its cleanup ran,
//     so it may have swapped some volumes and not others. Nothing on disk can
//     be trusted to represent a whole generation, and adopting it would freeze
//     a mixed state and bless it with a marker. Restore again. This mirrors
//     how backup.CleanStaging treats surviving staging as proof of a dead
//     process; the difference is that backup only reclaims the space, whereas
//     here the same signal also invalidates what is on disk.
//
//   - A marker means this node has served the Project before. Its volumes may
//     legitimately have moved ahead of every generation in the object store —
//     containers write continuously, backups only run on an interval — so
//     restoring would roll those writes back. Skip.
//
//   - No marker but data on disk is the awkward case: a Project that was born
//     on this node and never restored, or one that predates markers existing.
//     Restoring here would destroy live data, so the data is adopted and a
//     marker written to record ownership.
//
//   - Only when there is none of the above is a restore safe, because there is
//     nothing local to lose.
//
// Note that "no marker" alone is never sufficient grounds to restore.
func Decide(stagingPresent, markerPresent, volumesHaveData bool) Decision {
	switch {
	case stagingPresent:
		return DecisionRestore
	case markerPresent:
		return DecisionSkip
	case volumesHaveData:
		return DecisionAdoptExisting
	default:
		return DecisionRestore
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
