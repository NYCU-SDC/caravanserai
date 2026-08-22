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
	"time"

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

// ErrVersionConflict is returned when a compare-and-swap update finds that the
// resource's version moved since the caller read it.  The resource still
// exists and the request was not invalid — the caller simply lost a race.
//
// It is deliberately distinct from ErrConflictState: retry logic uses errors.Is
// to separate "read again and retry" from "this operation is not allowed", and
// folding them together would leave that decision with nothing to test.
var ErrVersionConflict = errors.New("resource version conflict")

// Store is the top-level persistence interface.  A single concrete type
// (e.g. sqlite.Store) implements all methods; tests may implement a subset
// via a narrow sub-interface or a hand-rolled stub.
type Store interface {
	NodeStore
	ProjectStore
	SecretStore
	PreAuthKeyStore
}

// ============================================================
// PreAuthKey
// ============================================================

// PreAuthKey state values. A key is issued as active and moves to used on the
// first successful heartbeat that consumes it. Expired/Revoked are reserved for
// later lifecycle work (CARA-49 revoke, GC sweeps) and are not written here.
const (
	PreAuthKeyStateActive  = "active"
	PreAuthKeyStateUsed    = "used"
	PreAuthKeyStateExpired = "expired"
	PreAuthKeyStateRevoked = "revoked"
)

// PreAuthKey is the server-side mapping between a Headscale pre-auth key and
// the Cara Node it was issued for (design CARA-50 §3–§4). It never holds the
// full pre-auth key: KeyHash is the hex SHA-256 of the key and KeyPrefix keeps
// only the first few characters for operator-facing audit.
type PreAuthKey struct {
	// KeyHash is the hex SHA-256 of the full pre-auth key. It is the durable
	// lookup key an agent's heartbeat references.
	KeyHash string
	// KeyPrefix is the first few characters of the raw key, retained only for
	// audit/log correlation. It is not sufficient to reconstruct the key.
	KeyPrefix string
	// CaraNodeName is the Cara Node this key authorises to join the overlay.
	CaraNodeName string
	// Expiration is when the key stops being valid. Zero means no expiry.
	Expiration time.Time
	// State is one of the PreAuthKeyState* constants.
	State string
	// UsedByIP is the overlay IP that consumed the key, set when State is used.
	UsedByIP string
	// UsedAt is when the key was consumed, set when State is used.
	UsedAt time.Time
	// IssuedBy records who requested the key, when available.
	IssuedBy string
}

// PreAuthKeyStore persists pre-auth key -> Cara Node mappings. The full
// pre-auth key is secret material and is never passed to or stored by this
// interface; callers hash it first.
type PreAuthKeyStore interface {
	// CreatePreAuthKey persists a new mapping. Returns ErrAlreadyExists if a
	// row with the same KeyHash already exists.
	CreatePreAuthKey(ctx context.Context, key *PreAuthKey) error

	// GetPreAuthKeyByHash returns the mapping for the given key hash.
	// Returns ErrNotFound if no such mapping exists.
	GetPreAuthKeyByHash(ctx context.Context, keyHash string) (*PreAuthKey, error)

	// MarkPreAuthKeyUsed transitions a key to the used state and records the
	// overlay IP and timestamp that consumed it. Returns ErrNotFound if the
	// key hash is unknown.
	MarkPreAuthKeyUsed(ctx context.Context, keyHash, usedByIP string, usedAt time.Time) error
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
	//
	// Deprecated: this replaces the whole status with no version check, so a
	// caller that read the Project earlier will silently discard anything
	// written in between.  Production read-modify-write paths must use
	// UpdateProjectStatusWithRetry.  Kept for tests that establish fixture
	// state, where no concurrent writer exists.
	UpdateProjectStatus(ctx context.Context, name string, status v1.ProjectStatus) error

	// UpdateProjectStatusWithRetry reads the named Project, applies mutate to a
	// copy of its status, and writes the result back guarded by the version it
	// read.  If another writer committed in between, it re-reads, re-applies
	// mutate to the new state, and tries again.
	//
	// At most 3 attempts in total: the first plus at most 2 retries.  A
	// conflict on the third returns ErrVersionConflict, leaving the caller's
	// own outer rhythm — the next reconcile, poll, or client retry — to pick
	// the work up again.  Only ErrVersionConflict is retried; everything else
	// returns immediately with its cause intact.
	//
	// mutate must be pure with respect to anything outside the status it is
	// handed.  It may be called more than once for a single logical update, so
	// any side effect inside it happens more than once too.  Values that must
	// stay fixed across attempts — timestamps above all — belong outside the
	// closure, captured by it.
	//
	// A mutate that leaves the status unchanged writes nothing, advances no
	// version, publishes no event, and returns nil.
	UpdateProjectStatusWithRetry(ctx context.Context, name string, mutate func(*v1.ProjectStatus) error) error

	// PatchProjectCondition replaces exactly one named condition inside a
	// Project's status.conditions, leaving phase and every other status field
	// untouched.  Used by the agent to publish Maintenance while controllers
	// concurrently write other status fields; a read-modify-write of the whole
	// status object would discard those.  A patch that changes nothing is a
	// no-op and publishes no event.  Returns ErrNotFound if the Project does
	// not exist.
	PatchProjectCondition(ctx context.Context, name string, condition v1.Condition) error

	// ClearProjectCondition removes the named condition from a Project's
	// status, with the same isolation guarantees as PatchProjectCondition.
	// Removing an absent condition is a no-op.
	ClearProjectCondition(ctx context.Context, name string, conditionType v1.ConditionType) error

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

// ============================================================
// Secret
// ============================================================

// SecretStore covers all Secret persistence operations.
//
// The interface is deliberately backend-agnostic: 1.0 stores Secrets in the
// same PostgreSQL resources table as everything else, but the backend is
// expected to move to Infisical before launch. Keeping this surface clean
// means that migration only swaps the implementation, not this interface or
// any caller.
type SecretStore interface {
	// CreateSecret persists a new Secret.  Returns ErrAlreadyExists if a
	// Secret with the same name already exists.
	CreateSecret(ctx context.Context, secret *v1.Secret) error

	// GetSecret returns the Secret with the given name, including its plaintext
	// values (the agent needs them to resolve secretKeyRef). Returns
	// ErrNotFound if it does not exist.
	GetSecret(ctx context.Context, name string) (*v1.Secret, error)

	// ListSecrets returns all Secrets in the store.
	ListSecrets(ctx context.Context) ([]*v1.Secret, error)

	// UpdateSecret replaces the full Secret record.  Used by the create-or-update
	// PUT path for credential rotation.  Returns ErrNotFound if it does not exist.
	UpdateSecret(ctx context.Context, secret *v1.Secret) error

	// DeleteSecret removes a Secret by name.
	// Returns ErrNotFound if it does not exist.
	DeleteSecret(ctx context.Context, name string) error
}
