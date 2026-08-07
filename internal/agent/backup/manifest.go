// Package backup implements the write side of Managed volume crash recovery
// (CARA-59): archiving Managed volume host directories, uploading them as a
// single Project-level generation, and maintaining the manifest that CARA-60
// restores from.
//
// This file holds the pure, orchestration-free pieces: key layout, ID
// generation, and the manifest/latest.json data model. Nothing here touches
// Docker, the object store, or a mutex — that lives in the BackupManager
// (stage 5), which calls into this file's functions from within its
// stop-containers/upload/restart workflow. Keeping this split lets the
// layout and manifest shape be unit-tested without a Docker daemon or a live
// S3 endpoint.
package backup

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"
)

// formatVersion is the manifest schema version. Bump it and branch on it in
// the reader (CARA-60) if the manifest shape ever needs to change
// incompatibly.
const formatVersion = 1

// backupMethod records how the archive was produced. 1.0 only ever stops the
// project and tars the volume directory; this field exists so a future
// online-backup method (per-service hooks, deferred — see CARA-59 Technical
// notes) can be introduced without an ambiguous manifest.
const backupMethod = "tar-offline"

// NewBackupID generates a new backup generation identifier. IDs are lexically
// sortable by creation time and unique per generation, but callers must never
// rely on that ordering to find the latest one — see Correction 2 in CARA-59:
// "latest" is only ever read from latest.json, never derived by listing keys.
//
// Format: {RFC3339-ish UTC timestamp with no separators}-{8 hex chars}, e.g.
// "20260724T140000Z-6e27c7b2".
func NewBackupID(now time.Time) (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("backup: generate backup ID suffix: %w", err)
	}
	return fmt.Sprintf("%s-%x", now.UTC().Format("20060102T150405Z"), suffix[:]), nil
}

// VolumeManifestEntry describes one Managed volume's archive within a backup
// generation.
type VolumeManifestEntry struct {
	// Name is the volume's name, matching v1.VolumeDef.Name.
	Name string `json:"name"`
	// ArchiveKey is the full object store key of this volume's tar.gz.
	ArchiveKey string `json:"archiveKey"`
	// SizeBytes is the archive's exact size, verified before extraction on
	// restore.
	SizeBytes int64 `json:"sizeBytes"`
	// SHA256 is the archive's checksum, verified before extraction on
	// restore.
	SHA256 string `json:"sha256"`
}

// Manifest describes one backup generation: every Managed volume captured
// together, from a single stop-the-project run. It is written only after
// every volume's archive has been uploaded and verified — see
// BuildManifest — and is immutable once written; latest.json is the only
// object a generation's completion updates afterward.
type Manifest struct {
	FormatVersion int `json:"formatVersion"`

	BackupID  string `json:"backupID"`
	Namespace string `json:"namespace"`
	Project   string `json:"project"`

	CreatedAt time.Time `json:"createdAt"`
	// SourceNode is the node that produced this generation, for operator
	// diagnosis; restore does not depend on it.
	SourceNode string `json:"sourceNode"`
	// ProjectResourceVersion pins the Project spec revision the volumes were
	// captured under, so an operator inspecting an old generation can tell
	// whether the spec has since changed.
	ProjectResourceVersion int64 `json:"projectResourceVersion"`

	CaraVersion  string `json:"caraVersion"`
	BackupMethod string `json:"backupMethod"`

	Volumes []VolumeManifestEntry `json:"volumes"`
}

// BuildManifest assembles a Manifest for a completed generation. It performs
// no I/O and does not itself enforce "only after every volume succeeded" —
// the caller (BackupManager) must only call this once every VolumeManifestEntry
// it collected represents a verified upload.
func BuildManifest(backupID, namespace, project, sourceNode, caraVersion string, projectResourceVersion int64, now time.Time, volumes []VolumeManifestEntry) Manifest {
	return Manifest{
		FormatVersion:          formatVersion,
		BackupID:               backupID,
		Namespace:              namespace,
		Project:                project,
		CreatedAt:              now.UTC(),
		SourceNode:             sourceNode,
		ProjectResourceVersion: projectResourceVersion,
		CaraVersion:            caraVersion,
		BackupMethod:           backupMethod,
		Volumes:                volumes,
	}
}

// Marshal serialises the manifest to indented JSON for upload.
func (m Manifest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: marshal manifest: %w", err)
	}
	return b, nil
}

// UnmarshalManifest parses a manifest previously written by Marshal.
func UnmarshalManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("backup: unmarshal manifest: %w", err)
	}
	return m, nil
}

// Latest is the small, mutable pointer at the root of a Project's backup
// prefix. It is the only object CARA-59 overwrites in place — everything
// else (archives, manifest.json) is written once under a unique backupID and
// never modified. CARA-60 reads Latest first and never lists keys to guess
// the newest generation.
type Latest struct {
	SchemaVersion int    `json:"schemaVersion"`
	BackupID      string `json:"backupID"`
	// ManifestKey is the object store key of this generation's manifest.json.
	ManifestKey string    `json:"manifestKey"`
	CreatedAt   time.Time `json:"createdAt"`
}

// NewLatest builds the Latest pointer for a just-completed generation.
func NewLatest(backupID, manifestKey string, now time.Time) Latest {
	return Latest{
		SchemaVersion: formatVersion,
		BackupID:      backupID,
		ManifestKey:   manifestKey,
		CreatedAt:     now.UTC(),
	}
}

// Marshal serialises the pointer to indented JSON for upload.
func (l Latest) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("backup: marshal latest: %w", err)
	}
	return b, nil
}

// UnmarshalLatest parses a latest.json previously written by Marshal.
func UnmarshalLatest(data []byte) (Latest, error) {
	var l Latest
	if err := json.Unmarshal(data, &l); err != nil {
		return Latest{}, fmt.Errorf("backup: unmarshal latest: %w", err)
	}
	return l, nil
}
