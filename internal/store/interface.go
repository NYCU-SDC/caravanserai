// Package store defines the persistence interface for Caravanserai resources.
//
// Design principles:
//
//   - A single Store interface covers all resource kinds.  Controllers and
//     handlers declare their own narrow sub-interfaces (e.g. NodeStore,
//     SchedulerProjectStore) that the concrete Store automatically satisfies
//     via Go's implicit interface implementation.
//
//   - Methods are context-aware so the SQLite implementation can respect
//     cancellation and deadlines from the Controller Manager.
//
//   - The interface uses api/v1 types directly so there is no translation
//     layer between the store and the rest of the codebase.
package store

import (
	"context"
	"errors"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// ErrNotFound is returned when a requested resource does not exist.
var ErrNotFound = errors.New("resource not found")

// ErrAlreadyExists is returned when a Create call targets a name that is
// already in use.
var ErrAlreadyExists = errors.New("resource already exists")

// ErrConflictState is returned when an operation is not allowed because the
// resource is in a state that conflicts with the request (e.g. updating a
// Project spec while it is Running).
var ErrConflictState = errors.New("operation conflicts with current resource state")

// Store is the top-level persistence interface.  A single concrete type
// (e.g. sqlite.Store) implements all methods; tests may implement a subset
// via a narrow sub-interface or a hand-rolled stub.
type Store interface {
	NodeStore
	ProjectStore
}

// ============================================================
// Node
// ============================================================

// NodeStore covers all Node persistence operations.
type NodeStore interface {
	// CreateNode persists a new Node.  Returns ErrAlreadyExists if a Node
	// with the same name already exists.
	CreateNode(ctx context.Context, node *v1.Node) error

	// GetNode returns the Node with the given name.
	// Returns ErrNotFound if it does not exist.
	GetNode(ctx context.Context, name string) (*v1.Node, error)

	// ListNodes returns all Nodes in the store.
	ListNodes(ctx context.Context) ([]*v1.Node, error)

	// UpdateNode replaces the full Node record (spec + status).
	// Returns ErrNotFound if it does not exist.
	UpdateNode(ctx context.Context, node *v1.Node) error

	// DeleteNode removes a Node by name.
	// Returns ErrNotFound if it does not exist.
	DeleteNode(ctx context.Context, name string) error

	// UpdateNodeSpec writes only the user-mutable fields of a Node (spec,
	// labels, annotations). Status is preserved. Returns ErrNotFound if it
	// does not exist.
	UpdateNodeSpec(ctx context.Context, node *v1.Node) error

	// UpdateNodeStatus writes only the status sub-object of the named Node.
	// This is the preferred path for the Agent heartbeat and the
	// NodeHealthController to avoid overwriting Spec changes made concurrently
	// by the API server.
	UpdateNodeStatus(ctx context.Context, name string, status v1.NodeStatus) error
}

// ============================================================
// Project
// ============================================================

// ProjectStore covers all Project persistence operations.
type ProjectStore interface {
	// CreateProject persists a new Project.  Returns ErrAlreadyExists if a
	// Project with the same name already exists.
	CreateProject(ctx context.Context, project *v1.Project) error

	// GetProject returns the Project with the given name.
	// Returns ErrNotFound if it does not exist.
	GetProject(ctx context.Context, name string) (*v1.Project, error)

	// ListProjects returns all Projects in the store.
	ListProjects(ctx context.Context) ([]*v1.Project, error)

	// ListProjectsByPhase returns all Projects whose status.phase equals phase.
	ListProjectsByPhase(ctx context.Context, phase v1.ProjectPhase) ([]*v1.Project, error)

	// ListProjectsByPhases returns all Projects whose status.phase is one of
	// the given phases. It is equivalent to calling ListProjectsByPhase for
	// each phase and merging the results, but may be more efficient.
	ListProjectsByPhases(ctx context.Context, phases []v1.ProjectPhase) ([]*v1.Project, error)

	// UpdateProject replaces the full Project record (spec + status).
	// Returns ErrNotFound if it does not exist.
	UpdateProject(ctx context.Context, project *v1.Project) error

	// DeleteProject removes a Project by name.
	// Returns ErrNotFound if it does not exist.
	DeleteProject(ctx context.Context, name string) error

	// UpdateProjectStatus writes only the status sub-object of the named
	// Project.  Used by the Controller Manager to avoid overwriting Spec
	// changes made concurrently by the API server.
	UpdateProjectStatus(ctx context.Context, name string, status v1.ProjectStatus) error

	// UpdateProjectSpec writes only the user-mutable fields of a Project
	// (spec, labels, annotations). Status is preserved. The update is only
	// allowed when the project's current phase is Pending or Failed; returns
	// ErrConflictState if the project is in any other phase, and ErrNotFound
	// if it does not exist.
	UpdateProjectSpec(ctx context.Context, project *v1.Project) error

	// ListProjectsByNodeRef returns all Projects assigned to the given node
	// whose phase is one of the supplied phases.  Used by
	// ProjectReschedulerController to find work that needs to be moved or
	// force-terminated when a node goes NotReady.
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*v1.Project, error)
}
