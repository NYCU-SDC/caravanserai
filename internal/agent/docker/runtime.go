// Package docker provides the container runtime integration for cara-agent.
//
// Design goals:
//   - Wrap the Docker API behind a narrow Runtime interface so that the agent
//     loop can be tested without a live Docker daemon.
//   - One Project maps to one Docker bridge network.  Each ServiceDef inside
//     the project spec maps to exactly one container.
//   - Container names follow the deterministic format "{project}-{service}",
//     which allows idempotent reconciliation without persisting container IDs.
//   - Ephemeral volumes are created as Docker named volumes and removed when
//     RemoveProject is called.
package docker

import (
	"context"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// ContainerState holds the observed state of a single service container.
type ContainerState struct {
	// ServiceName is the name of the ServiceDef this container belongs to.
	ServiceName string

	// ContainerID is the full Docker container ID.
	ContainerID string

	// Status is the Docker-reported status string: "running", "exited",
	// "created", "paused", "restarting", "dead", etc.
	Status string

	// ExitCode is the last exit code of the container process.
	// Meaningful only when Status == "exited".
	ExitCode int
}

// Runtime is the contract between the agent reconcile loop and the container
// engine.  All methods must be safe for concurrent use.
type Runtime interface {
	// ReconcileProject ensures every container defined in project.Spec.Services
	// is running.  It is idempotent:
	//   - If a container does not exist it is created and started.
	//   - If a container exists and is running it is left untouched.
	//   - If a container exists but is stopped it is started.
	// The network and any Ephemeral volumes are also created on demand.
	ReconcileProject(ctx context.Context, project *v1.Project) error

	// RemoveProject tears down all resources that were created for the project:
	// containers (stop + remove), the bridge network, and Ephemeral volumes.
	// It is safe to call even if the project was only partially created.
	RemoveProject(ctx context.Context, projectName string, spec v1.ProjectSpec) error

	// InspectProject returns the current state of every service container for
	// the project.  If a container for a service does not exist yet, it is
	// omitted from the returned slice (the caller can detect this by comparing
	// len(result) with len(project.Spec.Services)).
	InspectProject(ctx context.Context, project *v1.Project) ([]ContainerState, error)

	// GetContainerIPs returns a map of serviceName → IP address for each
	// service container in the project. The IP is read from the container's
	// attachment to the project bridge network (cara-{projectName}).
	// Services whose containers do not exist or have no IP are omitted.
	GetContainerIPs(ctx context.Context, project *v1.Project) (map[string]string, error)
}
