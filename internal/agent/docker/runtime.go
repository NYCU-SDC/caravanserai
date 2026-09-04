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

// ProjectIdentity identifies one Project's Docker resources. Namespace is
// included even while the API still treats names as globally unique so a
// destructive runtime operation never broadens to another namespace.
type ProjectIdentity struct {
	Namespace string
	Name      string
}

func (p ProjectIdentity) String() string { return p.Namespace + "/" + p.Name }

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
	// Managed volume host directories are deliberately retained; their paths are
	// logged so an operator can reclaim them. namespace is needed to locate
	// those directories. It is safe to call even if the project was only
	// partially created.
	RemoveProject(ctx context.Context, namespace, projectName string, spec v1.ProjectSpec) error

	// InspectProject returns the current state of every service container for
	// the project.  If a container for a service does not exist yet, it is
	// omitted from the returned slice (the caller can detect this by comparing
	// len(result) with len(project.Spec.Services)).
	InspectProject(ctx context.Context, project *v1.Project) ([]ContainerState, error)

	// StopProject stops every service container without removing it, so the
	// containers can be started again by StartProject.  Containers are stopped
	// in reverse spec order, so a service is stopped before whatever it
	// depends on.  Used by the backup flow, which needs the volumes quiesced
	// but the containers intact.  Missing containers are not an error.
	StopProject(ctx context.Context, project *v1.Project) error

	// StartProject starts every existing service container in spec order,
	// undoing StopProject.  It does not create missing containers — that is
	// ReconcileProject's job.  Missing containers are not an error.
	StartProject(ctx context.Context, project *v1.Project) error

	// GetContainerIPs returns a map of serviceName → IP address for each
	// service container in the project. The IP is read from the container's
	// attachment to the project bridge network (cara-{projectName}).
	// Services whose containers do not exist or have no IP are omitted.
	GetContainerIPs(ctx context.Context, project *v1.Project) (map[string]string, error)

	// ListLocalProjects returns the identity of every project that has
	// containers on this host, discovered from the complete Cara ownership
	// labels rather than from any server response. Identities are unique and
	// include projects whose containers are stopped, so a project the control
	// plane no longer assigns here is still visible to the caller.
	//
	// This is what makes orphan detection possible: the reconcile list only
	// contains projects the server still assigns to this node, so a project
	// that moved away can only be found by asking Docker directly.
	ListLocalProjects(ctx context.Context) ([]ProjectIdentity, error)

	// StopOrphanProject stops, but does not remove, every Cara-owned container
	// for project. It is the reversible first stage after the control plane
	// confirms that this node no longer owns the Project.
	StopOrphanProject(ctx context.Context, project ProjectIdentity) error

	// RemoveOrphanProject tears down every Docker resource labelled for the
	// project — containers, the bridge network, and named volumes — without
	// needing its spec.
	//
	// RemoveProject cannot be used for an orphan: it derives volume handling
	// from the spec, and the spec of a project the server no longer returns is
	// exactly what the agent does not have. Working from labels alone also
	// keeps Managed volume data safe by construction, because Managed volumes
	// are host bind directories rather than Docker volumes and so are not
	// reachable through a label filter at all.
	RemoveOrphanProject(ctx context.Context, project ProjectIdentity) error
}
