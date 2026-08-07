// Package restore reads the backup generations written by
// internal/agent/backup and puts a Project's Managed volumes back on disk
// before its containers start.
//
// The dangerous direction here is not "failed to restore" — that is loud and
// recoverable. It is "restored when we should not have", which silently
// replaces newer local data with an older generation and is indistinguishable
// from data loss. Every decision in this package is biased against restoring.
package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	caravolume "NYCU-SDC/caravanserai/internal/agent/volume"
)

// markerFilename is the placement marker, stored at the Project directory
// level:
//
//	{dataRoot}/volumes/{namespace}/{project}/.cara-restore.json
//
// It sits alongside the per-volume directories rather than inside one, for two
// reasons: an atomic directory swap during restore replaces {volume}/data
// wholesale and would take the marker with it, and a later backup archives
// only {volume}/data, so a marker kept here can never be captured as volume
// data and shipped to the object store.
//
// The leading dot also guarantees it can never collide with a volume
// directory — volume names are validated as DNS-style names, which cannot
// begin with a dot.
const markerFilename = ".cara-restore.json"

// Marker records that this node has established local data for a Project.
//
// Its presence — not its contents — is what suppresses future restores. Once
// this node is serving a Project, the local volumes are authoritative: they
// may legitimately have moved ahead of any generation in the object store,
// and re-restoring would roll those changes back.
type Marker struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`

	// BackupID is the generation this node restored from, or empty when the
	// volumes were initialised empty because no backup existed yet.
	BackupID string `json:"backupID,omitempty"`

	// RestoredAt is when this node took ownership of the local data.
	RestoredAt time.Time `json:"restoredAt"`
}

// MarkerPath returns the marker's location for a Project.
func MarkerPath(dataRoot, namespace, project string) (string, error) {
	dir, err := caravolume.ProjectDir(dataRoot, namespace, project)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, markerFilename), nil
}

// ReadMarker loads the Project's marker. It reports (nil, nil) when no marker
// exists, which callers must treat as "this node has not established data for
// this Project" rather than as an error.
//
// A marker that exists but cannot be parsed is also reported as absent, with
// the parse error surfaced for logging: a corrupt marker must not wedge the
// Project permanently, and the recovery for it is the same as for no marker
// at all.
func ReadMarker(dataRoot, namespace, project string) (*Marker, error) {
	path, err := MarkerPath(dataRoot, namespace, project)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("restore: read marker %q: %w", path, err)
	}

	var m Marker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("restore: parse marker %q: %w", path, err)
	}
	return &m, nil
}

// WriteMarker records that this node now owns the Project's local data.
// backupID is the generation restored, or empty when the volumes were
// initialised empty.
//
// The write is atomic (temp file then rename) so a crash midway cannot leave a
// half-written marker that later parses as garbage.
func WriteMarker(dataRoot, namespace, project, backupID string, now time.Time) error {
	path, err := MarkerPath(dataRoot, namespace, project)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("restore: create marker dir for %q: %w", path, err)
	}

	body, err := json.MarshalIndent(Marker{
		Namespace:  namespace,
		Project:    project,
		BackupID:   backupID,
		RestoredAt: now.UTC(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("restore: marshal marker: %w", err)
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("restore: write marker %q: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("restore: commit marker %q: %w", path, err)
	}
	return nil
}
