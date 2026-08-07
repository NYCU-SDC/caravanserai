// Package volume derives and provisions the host directories that back
// Managed volumes.
//
// A Managed volume lives in a directory the agent owns and bind-mounts into
// containers. The path is always derived from (namespace, project, volume) —
// it is never supplied by the user, so a Project spec cannot point the agent
// at an arbitrary location on the host.
package volume

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

const (
	// volumesDir separates volume data from anything else the agent may keep
	// under the data root later (staging, markers, cache).
	volumesDir = "volumes"

	// dataDir is the leaf actually mounted into the container. Keeping it one
	// level below the volume directory leaves room for sibling metadata that
	// must not end up inside the container or inside a backup archive.
	dataDir = "data"
)

// HostPath returns the host directory that backs a Managed volume:
//
//	{dataRoot}/volumes/{namespace}/{project}/{volume}/data
//
// Every identifier is validated as a DNS-style name, which rejects empty
// strings, path separators and "..". The result is additionally checked to be
// inside dataRoot, so loosening name validation later cannot silently turn
// into a path traversal.
func HostPath(dataRoot, namespace, project, volumeName string) (string, error) {
	components := []struct {
		label string
		value string
	}{
		{"namespace", namespace},
		{"project", project},
		{"volume", volumeName},
	}
	for _, c := range components {
		if err := v1.ValidateName(c.value); err != nil {
			return "", fmt.Errorf("%s %q cannot be used in a volume path: %w", c.label, c.value, err)
		}
	}

	if dataRoot == "" {
		return "", fmt.Errorf("data root must not be empty")
	}
	if !filepath.IsAbs(dataRoot) {
		return "", fmt.Errorf("data root %q must be an absolute path", dataRoot)
	}

	root := filepath.Clean(dataRoot)
	path := filepath.Join(root, volumesDir, namespace, project, volumeName, dataDir)
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("derived volume path %q escapes data root %q", path, root)
	}
	return path, nil
}

// VolumesRoot returns the directory every Project's Managed volumes live
// under: {dataRoot}/volumes. Used by sweeps that need to walk the whole tree
// rather than address one Project.
func VolumesRoot(dataRoot string) (string, error) {
	if dataRoot == "" {
		return "", fmt.Errorf("data root must not be empty")
	}
	if !filepath.IsAbs(dataRoot) {
		return "", fmt.Errorf("data root %q must be an absolute path", dataRoot)
	}
	return filepath.Join(filepath.Clean(dataRoot), volumesDir), nil
}

// ProjectDir returns the directory holding every Managed volume of a Project:
//
//	{dataRoot}/volumes/{namespace}/{project}
//
// Deletion and disk accounting operate on this level.
func ProjectDir(dataRoot, namespace, project string) (string, error) {
	// Derive through HostPath with a placeholder volume so both helpers share
	// one set of validation and containment rules, then trim the two trailing
	// components.
	const placeholder = "x"
	path, err := HostPath(dataRoot, namespace, project, placeholder)
	if err != nil {
		return "", err
	}
	return filepath.Dir(filepath.Dir(path)), nil
}
