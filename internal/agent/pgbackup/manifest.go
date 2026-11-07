package pgbackup

import (
	"encoding/json"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// ManifestFormatVersion is the version of Manifest. Version 1 is the Offline
// layout under Project names, which nothing here reads or writes.
const ManifestFormatVersion = 2

// Manifest describes one backup run of a Project lifetime. It is written once,
// at v1.BackupManifestKey, before the run is reported to the server. Nothing
// restores from it yet: no latest pointer names it, and an unaccepted one
// (its assignment was replaced before the report) is never a restore source.
type Manifest struct {
	FormatVersion int    `json:"formatVersion"`
	BackupID      string `json:"backupID"`
	ProjectUID    string `json:"projectUID"`
	// Namespace and Name are for people; no key is derived from them.
	Namespace            string         `json:"namespace"`
	Name                 string         `json:"name"`
	NodeRef              string         `json:"nodeRef"`
	AssignmentGeneration int64          `json:"assignmentGeneration"`
	CaraVersion          string         `json:"caraVersion"`
	CreatedAt            time.Time      `json:"createdAt"`
	Items                []ManifestItem `json:"items"`
}

// ManifestItem is the backup of one volume.
type ManifestItem struct {
	Volume      string            `json:"volume"`
	Strategy    v1.BackupStrategy `json:"strategy"`
	Stanza      string            `json:"stanza"`
	BackupLabel string            `json:"backupLabel"`
	BackupType  string            `json:"backupType"`
	PGMajor     int               `json:"pgMajor"`
	StartedAt   time.Time         `json:"startedAt"`
	StoppedAt   time.Time         `json:"stoppedAt"`
}

// BuildManifest describes a run of project's current assignment.
func BuildManifest(project *v1.Project, backupID, caraVersion string, now time.Time, items []Item) Manifest {
	m := Manifest{
		FormatVersion: ManifestFormatVersion, BackupID: backupID, ProjectUID: project.ObjectMeta.UID,
		Namespace: project.Namespace, Name: project.Name,
		NodeRef: project.Status.NodeRef, AssignmentGeneration: project.Status.AssignmentGeneration,
		CaraVersion: caraVersion, CreatedAt: now.UTC(),
	}
	for _, it := range items {
		m.Items = append(m.Items, ManifestItem{
			Volume: it.Volume, Strategy: v1.BackupStrategyPostgresDifferential, Stanza: it.Stanza,
			BackupLabel: it.Label, BackupType: it.Type, PGMajor: it.PGMajor,
			StartedAt: it.Started, StoppedAt: it.Stopped,
		})
	}
	return m
}

// Marshal encodes the manifest.
func (m Manifest) Marshal() ([]byte, error) { return json.MarshalIndent(m, "", "  ") }
