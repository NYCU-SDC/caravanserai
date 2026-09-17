// Package restore reads the backup generations written by
// internal/agent/backup and puts a Project's Managed volumes back on disk
// before its containers start.
//
// The dangerous direction here is not "failed to restore" — that is loud and
// recoverable. It is "restored when we should not have", which silently
// replaces newer local data with an older generation and is indistinguishable
// from data loss. Every decision in this package is biased against restoring.
//
// The second dangerous direction, learned later, is "used what was already on
// disk when it belonged to something else". A Project can move between Nodes
// and can be deleted and recreated under the same name, so a directory found
// at the expected path proves nothing on its own. What decides is the
// provenance recorded beside it.
package restore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
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

// MarkerVersion is the schema this agent writes.
//
// Version 1 recorded only (namespace, project) and a backup ID. Its presence
// was read as proof that the local volumes were authoritative, which holds
// only while a Project never leaves the Node it was created on. Version 2
// records which Project lifetime and which grant of ownership produced the
// data, so a later placement can tell its own data from someone else's.
const MarkerVersion = 2

// Provenance says which assignment established a Node's local Managed volume
// data.
//
// The three identity fields answer three different questions, and none
// substitutes for another:
//
//   - ProjectUID — which lifetime. A Project deleted and recreated under the
//     same name is a different Project with different data.
//   - NodeName — which Node's copy this is. Written so a directory copied or
//     restored onto the wrong host cannot pass as local.
//   - AssignmentGeneration — which grant of ownership. A Project reassigned
//     A→B→A holds the same UID on the same Node across two generations, and
//     only the generation separates the copy from before the move from the
//     one that is current.
type Provenance struct {
	Namespace            string
	Project              string
	ProjectUID           string
	NodeName             string
	AssignmentGeneration int64

	// BackupID is the generation this data was restored from, or empty when
	// the volumes were initialised empty.
	//
	// It records where the data started, not what it is now: containers write
	// continuously while backups run on an interval, so local data is
	// routinely ahead of the generation named here. It must never be read as
	// "this directory equals that backup".
	BackupID string
}

// ProvenanceFor builds the provenance for a Project as this Node currently
// holds it.
func ProvenanceFor(p *v1.Project, nodeName, backupID string) Provenance {
	return Provenance{
		Namespace:            p.Namespace,
		Project:              p.Name,
		ProjectUID:           p.ObjectMeta.UID,
		NodeName:             nodeName,
		AssignmentGeneration: p.Status.AssignmentGeneration,
		BackupID:             backupID,
	}
}

// Marker is the on-disk record of Provenance.
//
// Version is read before anything else. A file written by an older agent
// parses into this struct with Version zero and every identity field empty,
// which is not the same as "belongs to nobody" — it means "cannot be
// determined", and the caller must treat it that way rather than filling in
// the current assignment.
type Marker struct {
	Version   int    `json:"version"`
	Namespace string `json:"namespace"`
	Project   string `json:"project"`

	ProjectUID           string `json:"projectUID,omitempty"`
	NodeName             string `json:"nodeName,omitempty"`
	AssignmentGeneration int64  `json:"assignmentGeneration,omitempty"`

	// InitializedFromBackupID is Provenance.BackupID. The name says what it
	// means: where this data came from when it was established.
	InitializedFromBackupID string `json:"initializedFromBackupID,omitempty"`

	// EstablishedAt is when this node took ownership of the local data.
	EstablishedAt time.Time `json:"establishedAt"`
}

// IsLegacy reports whether the marker predates provenance and therefore says
// nothing about which assignment produced the data.
func (m *Marker) IsLegacy() bool { return m == nil || m.Version < MarkerVersion }

// Matches reports whether the marker proves the data belongs to want.
//
// All three identity fields must match. A partial match is not a weaker yes:
// the same UID on the same Node under a different generation is exactly the
// A→B→A case, where the data predates a period when another Node owned the
// Project and may have moved it on.
func (m *Marker) Matches(want Provenance) bool {
	if m.IsLegacy() {
		return false
	}
	return m.ProjectUID == want.ProjectUID &&
		m.NodeName == want.NodeName &&
		m.AssignmentGeneration == want.AssignmentGeneration
}

// Mismatches names the identity fields that differ from want, for logs and
// conditions. It returns nil when the marker matches, and for a legacy marker
// reports the version rather than a field-by-field diff there is no basis for.
func (m *Marker) Mismatches(want Provenance) []string {
	switch {
	case m == nil:
		return []string{"marker: absent"}
	case m.IsLegacy():
		return []string{fmt.Sprintf("version: %d, want %d", m.Version, MarkerVersion)}
	}

	var out []string
	if m.ProjectUID != want.ProjectUID {
		out = append(out, fmt.Sprintf("projectUID: %q, want %q", m.ProjectUID, want.ProjectUID))
	}
	if m.NodeName != want.NodeName {
		out = append(out, fmt.Sprintf("nodeName: %q, want %q", m.NodeName, want.NodeName))
	}
	if m.AssignmentGeneration != want.AssignmentGeneration {
		out = append(out, fmt.Sprintf("assignmentGeneration: %d, want %d",
			m.AssignmentGeneration, want.AssignmentGeneration))
	}
	return out
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
// exists, which callers must treat as "this node cannot prove it established
// data for this Project" rather than as an error.
//
// A marker that exists but cannot be parsed is an error, not an absence. The
// two used to be equivalent because the marker's only meaning was its
// presence; now its contents decide whether local data may be used, and a
// caller that cannot read them has to block rather than proceed as though the
// file were not there.
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
//
// It must be called only after the data it describes is in place: after a
// complete restore has been verified and swapped, or after an empty
// initialisation. A marker written earlier would claim provenance for
// whatever happened to be on disk.
//
// The write is atomic (temp file then rename) so a crash midway cannot leave a
// half-written marker that later parses as garbage.
func WriteMarker(dataRoot string, prov Provenance, now time.Time) error {
	path, err := MarkerPath(dataRoot, prov.Namespace, prov.Project)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("restore: create marker dir for %q: %w", path, err)
	}

	body, err := json.MarshalIndent(Marker{
		Version:                 MarkerVersion,
		Namespace:               prov.Namespace,
		Project:                 prov.Project,
		ProjectUID:              prov.ProjectUID,
		NodeName:                prov.NodeName,
		AssignmentGeneration:    prov.AssignmentGeneration,
		InitializedFromBackupID: prov.BackupID,
		EstablishedAt:           now.UTC(),
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
