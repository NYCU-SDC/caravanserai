package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
)

// quarantineRoot is the directory all quarantined Managed data lives under. It
// is outside the volumes tree so nothing can mistake it for live data, and on
// the same filesystem so moving there is a rename rather than a copy.
const quarantineRoot = "quarantine"

// Quarantine moves a released assignment's Managed data out of the active path
// (ADR-0024 C2), leaving nothing there and so no marker. The next placement on
// this Node then finds no local data and restores from the committed backup.
//
// The whole Project directory, marker included, moves in one rename, so a crash
// leaves the data either fully in place or fully moved, and never part of it at
// the active path. It is idempotent: with nothing at the active path there is
// nothing left to do.
//
// The caller must already have confirmed that no container of the assignment
// remains, and must hold the Project's Coordinator claim.
//
// Data whose marker names another assignment is not this obligation's, and is
// left alone. Data with an unreadable, legacy or absent marker cannot be shown
// to be someone else's, and moving it destroys nothing, so it is moved.
//
// Nothing here deletes data. How long quarantined data is kept is undecided.
func Quarantine(dataRoot string, owner Provenance, now time.Time) error {
	dir, err := caravolume.ProjectDir(dataRoot, owner.Namespace, owner.Project)
	if err != nil {
		return err
	}
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("restore: inspect %q: %w", dir, err)
	}

	marker, err := ReadMarker(dataRoot, owner.Namespace, owner.Project)
	if err == nil && marker != nil && !marker.IsLegacy() && !marker.Matches(owner) {
		return nil
	}

	parent := filepath.Join(filepath.Clean(dataRoot), quarantineRoot, owner.Namespace, owner.Project)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("restore: create quarantine dir %q: %w", parent, err)
	}
	dest := filepath.Join(parent, fmt.Sprintf("%s-gen%d-%s", owner.ProjectUID, owner.AssignmentGeneration, now.UTC().Format("20060102T150405Z")))
	if err := os.Rename(dir, dest); err != nil {
		return fmt.Errorf("restore: move %q to quarantine: %w", dir, err)
	}
	return nil
}
