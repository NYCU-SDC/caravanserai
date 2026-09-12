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
	"errors"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// ErrRecoveryVolumeUnavailable reports that a volume a service needs is not on
// this node: a Managed volume's host directory is gone, unreadable, or not a
// directory, or an Ephemeral volume's Docker named volume no longer exists.
//
// It is a distinct error because it calls for a distinct response. A container
// that failed to start may well start on the next attempt; missing data will
// not reappear on its own, so retrying is pointless and counting the failure
// towards exhaustion would report the wrong fault. What resolves it is a
// restore or an operator, and until then recovery has nothing useful to do.
var ErrRecoveryVolumeUnavailable = errors.New("recovery volume unavailable")

// VolumeUnavailableError is the concrete error behind
// ErrRecoveryVolumeUnavailable. errors.Is matches it against the sentinel;
// errors.As recovers which volume of which service is missing.
//
// It keeps two audiences apart. Service, Volume and Type are safe to publish
// through the API — they name things the Project's own spec already names.
// Detail describes the problem in node-local terms, a host path and the
// operating-system error, and belongs in the agent's log only: it would
// otherwise put this node's filesystem layout into Project status, where
// anyone who can read the Project can see it.
type VolumeUnavailableError struct {
	Service string
	Volume  string
	Type    v1.VolumeType
	Detail  string
}

func (e *VolumeUnavailableError) Error() string {
	return fmt.Sprintf("%s: service %q: %s volume %q: %s",
		ErrRecoveryVolumeUnavailable, e.Service, e.Type, e.Volume, e.Detail)
}

func (e *VolumeUnavailableError) Unwrap() error { return ErrRecoveryVolumeUnavailable }

// ErrContainerNotOwned reports that the container found under a service's
// deterministic name does not belong to the assignment being recovered: it
// carries another Project's labels, or — with UID enforcement on — another
// lifetime's UID or an earlier grant's assignment generation.
//
// Container names are derived from (project, service), so a name alone proves
// nothing about ownership: a container left by generation 7 has exactly the
// name generation 8 would use. Starting it would run a stale assignment's
// container as the current one, and removing it would destroy something this
// assignment does not own. Either bypasses the fence CARA-83 exists for, so
// recovery refuses both and leaves the container where it is.
//
// What removes it depends on which label differs. A container from a previous
// lifetime carries another UID, so under UID enforcement the orphan sweep sees
// it as another Project's and reclaims it. One left by an earlier grant of the
// same lifetime carries the same UID, so the sweep sees the Project it still
// has assigned here and leaves it; nothing reclaims such a container yet, and
// until that is decided an operator has to remove it.
var ErrContainerNotOwned = errors.New("container not owned by the current assignment")

// ContainerNotOwnedError is the concrete error behind ErrContainerNotOwned. It
// names the first ownership label that did not match.
type ContainerNotOwnedError struct {
	Container string
	Service   string
	Label     string
	Want      string
	Got       string
}

func (e *ContainerNotOwnedError) Error() string {
	return fmt.Sprintf("%s: container %q for service %q: label %s is %q, current assignment requires %q",
		ErrContainerNotOwned, e.Container, e.Service, e.Label, e.Got, e.Want)
}

func (e *ContainerNotOwnedError) Unwrap() error { return ErrContainerNotOwned }

// StaleContainer is a container labelled for a Project that does not belong to
// the Project's current assignment.
type StaleContainer struct {
	// Name is the Docker container name.
	Name string
	// Service is the cara.service label. It may name a service the current
	// spec no longer declares — which is exactly the case a per-service check
	// cannot find.
	Service string
	// ID is the full Docker container ID.
	ID string
	// Reason is the *ContainerNotOwnedError explaining which label differs.
	Reason error
}

// ProjectIdentity identifies one Project's Docker resources. Namespace is
// included even while the API still treats names as globally unique so a
// destructive runtime operation never broadens to another namespace.
//
// UID is the immutable server-generated Project identity (CARA-82). It fences
// ownership across Project lifetimes: a leftover container from a previous
// lifetime of the same (namespace, name) carries a different UID, and a pre-UID
// (legacy) container carries an empty UID. UID is empty for identities built in
// compatibility mode, where ownership falls back to (namespace, name).
type ProjectIdentity struct {
	Namespace string
	Name      string
	UID       string
}

func (p ProjectIdentity) String() string {
	if p.UID == "" {
		return p.Namespace + "/" + p.Name
	}
	return p.Namespace + "/" + p.Name + "@" + p.UID
}

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

	// NotOwned is nil when the container belongs to the assignment it was
	// inspected for, and otherwise a *ContainerNotOwnedError naming the label
	// that did not match. Status and ExitCode describe the container either
	// way, but a caller must not treat a container it does not own as the
	// Project's: a stale container that is running says nothing about whether
	// the current assignment is healthy.
	NotOwned error
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
	// len(result) with len(project.Spec.Services)). Containers are found by
	// name, so each state also reports, in NotOwned, whether the container
	// found under that name belongs to the project's current assignment.
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

	// RecoverServices puts the named services back, and touches nothing else.
	//
	// It exists because ReconcileProject cannot be used on a Project that is
	// already serving traffic: when any step fails it rolls back by calling
	// RemoveProject, which deletes every container, the network and the
	// Ephemeral volumes. That is right for a Project being created — the
	// rollback removes a half-built thing — and catastrophic for one being
	// repaired, where a failure to recreate one container would take the
	// healthy ones and their data with it.
	//
	// So this method never rolls back. A container created but not yet started
	// is left in place; the next attempt starts it, because each service is
	// handled idempotently. Partial progress is a better outcome than
	// destroying what still works.
	//
	// Only containers are touched. A missing network is repaired because doing
	// so is idempotent and cannot lose data, but volumes are deliberately left
	// alone: recreating a missing Managed volume directory would silently
	// start a service against empty data, which is worse than failing to
	// start at all.
	//
	// Every existing container is checked against the assignment before it is
	// started, removed, or counted as already recovered: the project, service
	// and namespace labels always, and with UID enforcement on, the UID and
	// assignment generation too. A container that fails the check is left
	// exactly as it is and the call returns ErrContainerNotOwned.
	//
	// Because Docker creates a volume it cannot find — at start as readily as
	// at create — the call runs in two passes. The first only reads, checking
	// that every volume mounted by every named service is present; if one is
	// not, the whole call is refused before any container is started, removed
	// or built. The second performs the recovery. Splitting them is what stops
	// a Project from being left half-repaired because the second service's
	// data turned out to be gone. The decision about a missing volume —
	// restore from a backup, or accept the loss — belongs to the restore path
	// and the operator, not here.
	RecoverServices(ctx context.Context, project *v1.Project, services []string) error

	// PreflightRecovery runs RecoverServices' read-only checks and nothing
	// else. It answers one question for the caller: is a recovery attempt on
	// these services worth spending?
	//
	// It is an attempt-accounting gate, not a safety guarantee. Nothing it
	// verifies is still guaranteed when RecoverServices runs — a volume can be
	// deleted in between — which is why RecoverServices repeats every check
	// itself and why that repetition must not be removed as redundant. This
	// call decides whether to start; that one decides whether it is safe to
	// proceed.
	//
	// A failure caused by absent volume data wraps
	// ErrRecoveryVolumeUnavailable, which the caller should treat as "blocked,
	// waiting for a human or a restore" rather than as a failed attempt.
	PreflightRecovery(ctx context.Context, project *v1.Project, services []string) error

	// StaleContainers returns every container labelled for this Project that
	// does not belong to its current assignment, whether or not the current
	// spec still declares the service it was created for.
	//
	// InspectProject cannot answer this. It walks the spec, so a container
	// left by an earlier generation for a service the spec has since dropped
	// is invisible to it: the new assignment would start alongside a workload
	// from the old one, and the orphan sweep would not reclaim it either
	// because the Project is still assigned to this node. Finding these means
	// asking Docker for everything carrying the Project's labels and checking
	// each against the current assignment.
	//
	// An empty result means every container found is this assignment's.
	StaleContainers(ctx context.Context, project *v1.Project) ([]StaleContainer, error)

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
