package docker

import "fmt"

// NetworkName returns the Docker network name for a project.
// Format: "cara-{projectName}"
func NetworkName(projectName string) string {
	return fmt.Sprintf("cara-%s", projectName)
}

// ContainerName returns the deterministic container name for a service.
// Format: "{projectName}-{serviceName}"
//
// This naming convention allows the agent to locate a container by name
// without persisting the container ID anywhere — a simple ContainerInspect
// on the well-known name is sufficient.
func ContainerName(projectName, serviceName string) string {
	return fmt.Sprintf("%s-%s", projectName, serviceName)
}

// VolumeName returns the Docker volume name for a project-scoped volume.
// Format: "cara-{projectName}-{volumeName}"
func VolumeName(projectName, volumeName string) string {
	return fmt.Sprintf("cara-%s-%s", projectName, volumeName)
}
