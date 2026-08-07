package backup

import (
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// keyPrefix is the schema-versioned root for every object this package
// writes, mirroring the object store layout in the CARA design doc:
//
//	cara/v1/projects/{namespace}/{project}/
//	├── snapshots/{backupID}/
//	│   ├── {volume}.tar.gz
//	│   └── manifest.json
//	└── latest.json
const keyPrefix = "cara/v1/projects"

// ProjectPrefix returns the root key under which every generation of a
// Project's backups lives.
func ProjectPrefix(namespace, project string) (string, error) {
	if err := v1.ValidateName(namespace); err != nil {
		return "", fmt.Errorf("backup: namespace %q: %w", namespace, err)
	}
	if err := v1.ValidateName(project); err != nil {
		return "", fmt.Errorf("backup: project %q: %w", project, err)
	}
	return fmt.Sprintf("%s/%s/%s", keyPrefix, namespace, project), nil
}

// LatestKey returns the key of the small, mutable pointer to the newest
// complete generation. See Latest for why this is the only object CARA-59
// overwrites in place.
func LatestKey(namespace, project string) (string, error) {
	prefix, err := ProjectPrefix(namespace, project)
	if err != nil {
		return "", err
	}
	return prefix + "/latest.json", nil
}

// GenerationPrefix returns the key prefix for one backup generation: every
// volume archive and the manifest describing them.
func GenerationPrefix(namespace, project, backupID string) (string, error) {
	prefix, err := ProjectPrefix(namespace, project)
	if err != nil {
		return "", err
	}
	if backupID == "" {
		return "", fmt.Errorf("backup: backupID must not be empty")
	}
	return fmt.Sprintf("%s/snapshots/%s", prefix, backupID), nil
}

// ManifestKey returns the key of one generation's manifest.json.
func ManifestKey(namespace, project, backupID string) (string, error) {
	prefix, err := GenerationPrefix(namespace, project, backupID)
	if err != nil {
		return "", err
	}
	return prefix + "/manifest.json", nil
}

// ArchiveKey returns the key of one volume's archive within a generation.
// volumeName is validated the same way a Project's volume names are, so a
// malformed name can never produce a key that escapes the generation prefix.
func ArchiveKey(namespace, project, backupID, volumeName string) (string, error) {
	prefix, err := GenerationPrefix(namespace, project, backupID)
	if err != nil {
		return "", err
	}
	if err := v1.ValidateName(volumeName); err != nil {
		return "", fmt.Errorf("backup: volume %q: %w", volumeName, err)
	}
	return fmt.Sprintf("%s/%s.tar.gz", prefix, volumeName), nil
}
