// Package postgres provides a PostgreSQL-backed implementation of store.Store.
//
// Schema design:
//
//	A single "resources" table holds all resource kinds (Node, Project, …).
//	Adding a new kind never requires a schema migration — only a new Go
//	implementation of store.Store's methods for that kind.
//
//	Each row stores:
//	  kind        — resource kind string ("Node", "Project", …)
//	  name        — resource name (metadata.name), unique within a kind
//	  phase       — promoted text column for cheap filtered list queries
//	  spec        — JSONB, the desired state written by users / the API server
//	  status      — JSONB, the observed state written by the Controller Manager
//	  labels      — JSONB, promoted for future label-selector GIN queries
//	  annotations — JSONB
//	  created_at / updated_at — timestamps
//
//	spec and status are stored separately so UpdateNodeStatus / UpdateProjectStatus
//	can issue a targeted UPDATE of only the status column, avoiding the
//	read-modify-write round-trip that a single data-blob design requires.
//
// Migrations:
//
//	SQL files live in migrations/ at the module root and are embedded into
//	the binary via go:embed.  On startup, New() calls MigrateUp() which runs
//	any pending migrations using golang-migrate with the iofs source driver.
//
// Connection:
//
//	New() accepts a standard PostgreSQL URL
//	(postgres://user:pass@host:5432/dbname?sslmode=disable).
//	Internally it uses pgx/v5 via the pgxpool driver for connection pooling.
package postgres

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

const (
	kindNode    = "Node"
	kindProject = "Project"
	kindSecret  = "Secret"

	// defaultNamespace is the only namespace value 1.0 ever writes or reads.
	// Node rows are always written with this value even though NodeStore's
	// API surface doesn't expose namespace as a parameter (Node is
	// cluster-scoped). Project lookups by bare name (Get/Update/Delete/List)
	// filter on this constant since routes don't accept a namespace segment
	// yet; api/v1.ValidateNamespace rejects anything else before it reaches
	// the store.
	defaultNamespace = v1.DefaultNamespace
)

// Store is the PostgreSQL-backed implementation of store.Store.
type Store struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
	bus    *event.Bus
}

// New opens a connection pool to the PostgreSQL database at databaseURL,
// runs pending schema migrations, and returns a ready-to-use Store.
// bus may be nil; if so no events are published.
func New(ctx context.Context, databaseURL string, logger *zap.Logger, bus *event.Bus) (*Store, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	if err := migrateUp(databaseURL, logger); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: migrate: %w", err)
	}

	return &Store{pool: pool, logger: logger, bus: bus}, nil
}

// Close releases the connection pool.
func (s *Store) Close() {
	s.pool.Close()
}

// migrateUp runs all pending UP migrations embedded in the binary.
func migrateUp(databaseURL string, logger *zap.Logger) error {
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("iofs source: %w", err)
	}

	m, err := migrate.NewWithSourceInstance("iofs", src, databaseURL)
	if err != nil {
		return fmt.Errorf("migrate new: %w", err)
	}
	m.Log = &migrateLogger{logger: logger}

	version, dirty, verErr := m.Version()
	if verErr != nil && !errors.Is(verErr, migrate.ErrNilVersion) {
		return fmt.Errorf("migrate version: %w", verErr)
	}
	if version == 0 {
		logger.Info("No existing database version detected, running migrations")
	} else {
		logger.Info("Current migration version",
			zap.Uint("version", version),
			zap.Bool("dirty", dirty),
		)
	}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logger.Info("Database schema is up to date")
			return nil
		}
		return fmt.Errorf("migrate up: %w", err)
	}

	logger.Info("Database migration completed")
	return nil
}

// publish fires an event on the bus if one is configured.
// It is a no-op when s.bus is nil.
func (s *Store) publish(topic event.Topic, name string) {
	if s.bus != nil {
		s.bus.Publish(topic, name)
	}
}

// ============================================================
// NodeStore
// ============================================================

// CreateNode implements store.NodeStore.
func (s *Store) CreateNode(ctx context.Context, node *v1.Node) error {
	now := time.Now().UTC()
	node.ObjectMeta.CreatedAt = now
	node.ObjectMeta.UpdatedAt = now

	spec, err := json.Marshal(node.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal node spec: %w", err)
	}
	status, err := json.Marshal(node.Status)
	if err != nil {
		return fmt.Errorf("postgres: marshal node status: %w", err)
	}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal node labels: %w", err)
	}
	annotations, err := json.Marshal(node.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal node annotations: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO resources (kind, name, namespace, phase, spec, status, labels, annotations, created_at, updated_at, resource_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1)`,
		kindNode, node.Name, defaultNamespace, string(node.Status.State),
		spec, status, labels, annotations,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: create node %q: %w", node.Name, err)
	}
	node.ObjectMeta.Namespace = defaultNamespace
	node.ObjectMeta.ResourceVersion = 1
	s.publish(event.TopicNodeCreated, node.Name)
	return nil
}

// GetNode implements store.NodeStore.
func (s *Store) GetNode(ctx context.Context, name string) (*v1.Node, error) {
	return s.getNode(ctx, name)
}

func (s *Store) getNode(ctx context.Context, name string) (*v1.Node, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND name = $2`,
		kindNode, name,
	)

	var (
		namespace                                     string
		resourceVersion                               int64
		rawSpec, rawStatus, rawLabels, rawAnnotations []byte
		createdAt, updatedAt                          time.Time
	)
	if err := row.Scan(&namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get node %q: %w", name, err)
	}

	node := &v1.Node{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindNode},
		ObjectMeta: v1.ObjectMeta{
			Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		},
	}
	if err := unmarshalFields(name, rawSpec, &node.Spec, rawStatus, &node.Status, rawLabels, &node.Labels, rawAnnotations, &node.Annotations); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal node %q: %w", name, err)
	}
	return node, nil
}

// ListNodes implements store.NodeStore.
func (s *Store) ListNodes(ctx context.Context) ([]*v1.Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1`, kindNode)
	if err != nil {
		return nil, fmt.Errorf("postgres: list nodes: %w", err)
	}
	defer rows.Close()

	var nodes []*v1.Node
	for rows.Next() {
		var (
			name, namespace                               string
			resourceVersion                               int64
			rawSpec, rawStatus, rawLabels, rawAnnotations []byte
			createdAt, updatedAt                          time.Time
		)
		if err := rows.Scan(&name, &namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan node row: %w", err)
		}
		node := &v1.Node{
			TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindNode},
			ObjectMeta: v1.ObjectMeta{
				Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
				CreatedAt: createdAt, UpdatedAt: updatedAt,
			},
		}
		if err := unmarshalFields(name, rawSpec, &node.Spec, rawStatus, &node.Status, rawLabels, &node.Labels, rawAnnotations, &node.Annotations); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal node %q: %w", name, err)
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// UpdateNode implements store.NodeStore.
func (s *Store) UpdateNode(ctx context.Context, node *v1.Node) error {
	node.ObjectMeta.UpdatedAt = time.Now().UTC()

	spec, err := json.Marshal(node.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal node spec: %w", err)
	}
	status, err := json.Marshal(node.Status)
	if err != nil {
		return fmt.Errorf("postgres: marshal node status: %w", err)
	}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal labels: %w", err)
	}
	annotations, err := json.Marshal(node.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal annotations: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		UPDATE resources
		SET phase = $1, spec = $2, status = $3, labels = $4, annotations = $5, updated_at = $6,
		    resource_version = resource_version + 1
		WHERE kind = $7 AND name = $8
		RETURNING resource_version`,
		string(node.Status.State), spec, status, labels, annotations,
		node.ObjectMeta.UpdatedAt, kindNode, node.Name,
	).Scan(&node.ObjectMeta.ResourceVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: node %q", store.ErrNotFound, node.Name)
		}
		return fmt.Errorf("postgres: update node %q: %w", node.Name, err)
	}
	return nil
}

// UpdateNodeSpec implements store.NodeStore.
// Only the spec, labels, annotations, and updated_at columns are written;
// status and phase are untouched.
func (s *Store) UpdateNodeSpec(ctx context.Context, node *v1.Node) error {
	now := time.Now().UTC()

	spec, err := json.Marshal(node.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal node spec: %w", err)
	}
	labels, err := json.Marshal(node.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal node labels: %w", err)
	}
	annotations, err := json.Marshal(node.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal node annotations: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		UPDATE resources
		SET spec = $1, labels = $2, annotations = $3, updated_at = $4,
		    resource_version = resource_version + 1
		WHERE kind = $5 AND name = $6
		RETURNING resource_version`,
		spec, labels, annotations, now, kindNode, node.Name,
	).Scan(&node.ObjectMeta.ResourceVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: node %q", store.ErrNotFound, node.Name)
		}
		return fmt.Errorf("postgres: update node spec %q: %w", node.Name, err)
	}
	s.publish(event.TopicNodeUpdated, node.Name)
	return nil
}

// DeleteNode implements store.NodeStore.
func (s *Store) DeleteNode(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM resources WHERE kind = $1 AND name = $2`, kindNode, name)
	if err != nil {
		return fmt.Errorf("postgres: delete node %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: node %q", store.ErrNotFound, name)
	}
	return nil
}

// UpdateNodeStatus implements store.NodeStore.
// Only the status column (and the promoted phase column) are written; spec is
// untouched, so concurrent API-server spec updates are not clobbered.
//
// A TopicNodeUpdated event is published only when the node's phase actually
// changes (e.g. Ready→NotReady), not on every heartbeat refresh.
func (s *Store) UpdateNodeStatus(ctx context.Context, name string, status v1.NodeStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("postgres: marshal node status: %w", err)
	}

	// Use a CTE to atomically capture the old phase before the UPDATE.
	// This avoids a separate SELECT and any TOCTOU race conditions.
	var oldPhase string
	err = s.pool.QueryRow(ctx, `
		WITH old AS (
			SELECT phase FROM resources WHERE kind = $4 AND name = $5
		)
		UPDATE resources
		SET phase = $1, status = $2, updated_at = $3, resource_version = resource_version + 1
		WHERE kind = $4 AND name = $5
		RETURNING (SELECT phase FROM old) AS old_phase`,
		string(status.State), raw, time.Now().UTC(), kindNode, name,
	).Scan(&oldPhase)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: node %q", store.ErrNotFound, name)
		}
		return fmt.Errorf("postgres: update node status %q: %w", name, err)
	}

	if oldPhase != string(status.State) {
		s.publish(event.TopicNodeUpdated, name)
	}
	return nil
}

// ============================================================
// ProjectStore
// ============================================================

// CreateProject implements store.ProjectStore.
func (s *Store) CreateProject(ctx context.Context, project *v1.Project) error {
	now := time.Now().UTC()
	project.ObjectMeta.CreatedAt = now
	project.ObjectMeta.UpdatedAt = now
	if project.ObjectMeta.Namespace == "" {
		project.ObjectMeta.Namespace = defaultNamespace
	}

	spec, err := json.Marshal(project.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal project spec: %w", err)
	}
	status, err := json.Marshal(project.Status)
	if err != nil {
		return fmt.Errorf("postgres: marshal project status: %w", err)
	}
	labels, err := json.Marshal(project.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal project labels: %w", err)
	}
	annotations, err := json.Marshal(project.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal project annotations: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO resources (kind, name, namespace, phase, spec, status, labels, annotations, created_at, updated_at, resource_version)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 1)`,
		kindProject, project.Name, project.ObjectMeta.Namespace, string(project.Status.Phase),
		spec, status, labels, annotations,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: create project %q: %w", project.Name, err)
	}
	project.ObjectMeta.ResourceVersion = 1
	s.publish(event.TopicProjectCreated, project.Name)
	return nil
}

// GetProject implements store.ProjectStore.
func (s *Store) GetProject(ctx context.Context, name string) (*v1.Project, error) {
	return s.getProject(ctx, name)
}

func (s *Store) getProject(ctx context.Context, name string) (*v1.Project, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND namespace = $2 AND name = $3`,
		kindProject, defaultNamespace, name,
	)

	var (
		namespace                                     string
		resourceVersion                               int64
		rawSpec, rawStatus, rawLabels, rawAnnotations []byte
		createdAt, updatedAt                          time.Time
	)
	if err := row.Scan(&namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get project %q: %w", name, err)
	}

	project := &v1.Project{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindProject},
		ObjectMeta: v1.ObjectMeta{
			Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		},
	}
	if err := unmarshalFields(name, rawSpec, &project.Spec, rawStatus, &project.Status, rawLabels, &project.Labels, rawAnnotations, &project.Annotations); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal project %q: %w", name, err)
	}
	return project, nil
}

// ListProjects implements store.ProjectStore.
func (s *Store) ListProjects(ctx context.Context) ([]*v1.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND namespace = $2`, kindProject, defaultNamespace)
	if err != nil {
		return nil, fmt.Errorf("postgres: list projects: %w", err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

// ListProjectsByPhase implements store.ProjectStore.
// Uses the promoted phase column + idx_resources_kind_phase index.
func (s *Store) ListProjectsByPhase(ctx context.Context, phase v1.ProjectPhase) ([]*v1.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND namespace = $2 AND phase = $3`,
		kindProject, defaultNamespace, string(phase),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list projects by phase %q: %w", phase, err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

// ListProjectsByPhases implements store.ProjectStore.
// Returns all Projects whose phase is one of the given phases.
// Uses a single query with = ANY($2) to hit the kind_phase index efficiently.
func (s *Store) ListProjectsByPhases(ctx context.Context, phases []v1.ProjectPhase) ([]*v1.Project, error) {
	if len(phases) == 0 {
		return nil, nil
	}

	phaseStrings := make([]string, len(phases))
	for i, p := range phases {
		phaseStrings[i] = string(p)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND namespace = $2 AND phase = ANY($3)`,
		kindProject, defaultNamespace, phaseStrings,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list projects by phases %v: %w", phases, err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

// ListProjectsByNodeRef implements store.ProjectStore.
// Returns all Projects assigned to nodeRef whose phase is one of phases.
// nodeRef is stored inside the status JSONB column (not a promoted column),
// so we query it with the ->> operator. The promoted phase column is still
// used for the phase filter, keeping that part index-friendly.
func (s *Store) ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*v1.Project, error) {
	if len(phases) == 0 {
		return nil, nil
	}

	phaseStrings := make([]string, len(phases))
	for i, p := range phases {
		phaseStrings[i] = string(p)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND namespace = $2 AND phase = ANY($3) AND status->>'nodeRef' = $4`,
		kindProject, defaultNamespace, phaseStrings, nodeRef,
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: list projects by node_ref %q phases %v: %w", nodeRef, phases, err)
	}
	defer rows.Close()

	return scanProjects(rows)
}

// UpdateProject implements store.ProjectStore.
func (s *Store) UpdateProject(ctx context.Context, project *v1.Project) error {
	project.ObjectMeta.UpdatedAt = time.Now().UTC()
	if project.ObjectMeta.Namespace == "" {
		project.ObjectMeta.Namespace = defaultNamespace
	}

	spec, err := json.Marshal(project.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal project spec: %w", err)
	}
	status, err := json.Marshal(project.Status)
	if err != nil {
		return fmt.Errorf("postgres: marshal project status: %w", err)
	}
	labels, err := json.Marshal(project.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal labels: %w", err)
	}
	annotations, err := json.Marshal(project.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal annotations: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		UPDATE resources
		SET phase = $1, spec = $2, status = $3, labels = $4, annotations = $5, updated_at = $6,
		    resource_version = resource_version + 1
		WHERE kind = $7 AND namespace = $8 AND name = $9
		RETURNING resource_version`,
		string(project.Status.Phase), spec, status, labels, annotations,
		project.ObjectMeta.UpdatedAt, kindProject, project.ObjectMeta.Namespace, project.Name,
	).Scan(&project.ObjectMeta.ResourceVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: project %q", store.ErrNotFound, project.Name)
		}
		return fmt.Errorf("postgres: update project %q: %w", project.Name, err)
	}
	return nil
}

// DeleteProject implements store.ProjectStore.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM resources WHERE kind = $1 AND namespace = $2 AND name = $3`, kindProject, defaultNamespace, name)
	if err != nil {
		return fmt.Errorf("postgres: delete project %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: project %q", store.ErrNotFound, name)
	}
	return nil
}

// UpdateProjectStatus implements store.ProjectStore.
// Only the status column (and promoted phase) are written; spec is untouched.
func (s *Store) UpdateProjectStatus(ctx context.Context, name string, status v1.ProjectStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("postgres: marshal project status: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE resources
		SET phase = $1, status = $2, updated_at = $3, resource_version = resource_version + 1
		WHERE kind = $4 AND namespace = $5 AND name = $6`,
		string(status.Phase), raw, time.Now().UTC(), kindProject, defaultNamespace, name,
	)
	if err != nil {
		return fmt.Errorf("postgres: update project status %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: project %q", store.ErrNotFound, name)
	}
	s.publish(event.TopicProjectUpdated, name)
	return nil
}

// conditionsExcludingType is a SQL expression yielding a Project's
// status.conditions array with any entry of type $4 removed. It is the shared
// half of both the merge and clear paths below.
const conditionsExcludingType = `COALESCE(
		(SELECT jsonb_agg(c)
		 FROM jsonb_array_elements(COALESCE(status->'conditions', '[]'::jsonb)) c
		 WHERE c->>'type' <> $4),
		'[]'::jsonb)`

// PatchProjectCondition implements store.ProjectStore.
//
// It replaces exactly one named condition inside status.conditions, leaving
// phase and every other status field untouched. The merge happens inside a
// single UPDATE rather than as a read-modify-write in Go: the agent writes
// Maintenance while controllers concurrently write nodeRef and phase onto the
// same row, and reading the whole status object to rewrite it would silently
// discard whichever write landed in between.
//
// A patch that would not change the stored status is a no-op: no version
// bump, and no event. Otherwise a backup that re-asserted the same condition
// would wake every subscriber for nothing.
func (s *Store) PatchProjectCondition(ctx context.Context, name string, condition v1.Condition) error {
	raw, err := json.Marshal([]v1.Condition{condition})
	if err != nil {
		return fmt.Errorf("postgres: marshal condition: %w", err)
	}

	newStatus := fmt.Sprintf("jsonb_set(status, '{conditions}', %s || $5::jsonb)", conditionsExcludingType)
	query := fmt.Sprintf(`
		UPDATE resources
		SET status = %s, updated_at = $6, resource_version = resource_version + 1
		WHERE kind = $1 AND namespace = $2 AND name = $3
		  AND status IS DISTINCT FROM %s`, newStatus, newStatus)

	tag, err := s.pool.Exec(ctx, query,
		kindProject, defaultNamespace, name, string(condition.Type), raw, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("postgres: patch project condition %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return s.conditionPatchNoOp(ctx, name)
	}

	s.publish(event.TopicProjectUpdated, name)
	return nil
}

// ClearProjectCondition implements store.ProjectStore.
//
// It removes the named condition, again without touching phase or any other
// status field, and is a no-op when the condition is already absent.
func (s *Store) ClearProjectCondition(ctx context.Context, name string, conditionType v1.ConditionType) error {
	newStatus := fmt.Sprintf("jsonb_set(status, '{conditions}', %s)", conditionsExcludingType)
	query := fmt.Sprintf(`
		UPDATE resources
		SET status = %s, updated_at = $5, resource_version = resource_version + 1
		WHERE kind = $1 AND namespace = $2 AND name = $3
		  AND status IS DISTINCT FROM %s`, newStatus, newStatus)

	tag, err := s.pool.Exec(ctx, query,
		kindProject, defaultNamespace, name, string(conditionType), time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("postgres: clear project condition %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return s.conditionPatchNoOp(ctx, name)
	}

	s.publish(event.TopicProjectUpdated, name)
	return nil
}

// conditionPatchNoOp resolves the two reasons a condition patch can affect no
// rows: the Project does not exist, or the patch changed nothing. Only the
// former is an error.
func (s *Store) conditionPatchNoOp(ctx context.Context, name string) error {
	if _, err := s.getProject(ctx, name); err != nil {
		return fmt.Errorf("%w: project %q", store.ErrNotFound, name)
	}
	return nil
}

// UpdateProjectSpec implements store.ProjectStore.
// Only the spec, labels, annotations, and updated_at columns are written;
// status and phase are untouched. The update is only allowed when the
// project's current phase is Pending or Failed; returns ErrConflictState
// otherwise.
func (s *Store) UpdateProjectSpec(ctx context.Context, project *v1.Project) error {
	now := time.Now().UTC()
	if project.ObjectMeta.Namespace == "" {
		project.ObjectMeta.Namespace = defaultNamespace
	}

	spec, err := json.Marshal(project.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal project spec: %w", err)
	}
	labels, err := json.Marshal(project.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal project labels: %w", err)
	}
	annotations, err := json.Marshal(project.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal project annotations: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE resources
		SET spec = $1, labels = $2, annotations = $3, updated_at = $4, resource_version = resource_version + 1
		WHERE kind = $5 AND namespace = $6 AND name = $7 AND phase = ANY($8)`,
		spec, labels, annotations, now, kindProject, project.ObjectMeta.Namespace, project.Name,
		[]string{string(v1.ProjectPhasePending), string(v1.ProjectPhaseFailed)},
	)
	if err != nil {
		return fmt.Errorf("postgres: update project spec %q: %w", project.Name, err)
	}
	if tag.RowsAffected() == 0 {
		// Distinguish "not found" from "wrong phase" by checking existence.
		_, getErr := s.getProject(ctx, project.Name)
		if getErr != nil {
			return fmt.Errorf("%w: project %q", store.ErrNotFound, project.Name)
		}
		return fmt.Errorf("%w: project %q is not in Pending or Failed phase", store.ErrConflictState, project.Name)
	}
	s.publish(event.TopicProjectUpdated, project.Name)
	return nil
}

// ============================================================
// SecretStore
// ============================================================

// Secrets share the resources table with everything else (kind='Secret').
// They have no lifecycle phase, so the promoted phase column is always the
// empty string, and no events are published — nothing subscribes to Secret
// changes. Namespace/resource_version handling mirrors ProjectStore.

// CreateSecret implements store.SecretStore.
func (s *Store) CreateSecret(ctx context.Context, secret *v1.Secret) error {
	now := time.Now().UTC()
	secret.ObjectMeta.CreatedAt = now
	secret.ObjectMeta.UpdatedAt = now
	if secret.ObjectMeta.Namespace == "" {
		secret.ObjectMeta.Namespace = defaultNamespace
	}

	spec, err := json.Marshal(secret.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret spec: %w", err)
	}
	status, err := json.Marshal(secret.Status)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret status: %w", err)
	}
	labels, err := json.Marshal(secret.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret labels: %w", err)
	}
	annotations, err := json.Marshal(secret.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret annotations: %w", err)
	}

	_, err = s.pool.Exec(ctx, `
		INSERT INTO resources (kind, name, namespace, phase, spec, status, labels, annotations, created_at, updated_at, resource_version)
		VALUES ($1, $2, $3, '', $4, $5, $6, $7, $8, $9, 1)`,
		kindSecret, secret.Name, secret.ObjectMeta.Namespace,
		spec, status, labels, annotations,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: create secret %q: %w", secret.Name, err)
	}
	secret.ObjectMeta.ResourceVersion = 1
	return nil
}

// GetSecret implements store.SecretStore.
func (s *Store) GetSecret(ctx context.Context, name string) (*v1.Secret, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND namespace = $2 AND name = $3`,
		kindSecret, defaultNamespace, name,
	)

	var (
		namespace                                     string
		resourceVersion                               int64
		rawSpec, rawStatus, rawLabels, rawAnnotations []byte
		createdAt, updatedAt                          time.Time
	)
	if err := row.Scan(&namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get secret %q: %w", name, err)
	}

	secret := &v1.Secret{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindSecret},
		ObjectMeta: v1.ObjectMeta{
			Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
			CreatedAt: createdAt, UpdatedAt: updatedAt,
		},
	}
	if err := unmarshalFields(name, rawSpec, &secret.Spec, rawStatus, &secret.Status, rawLabels, &secret.Labels, rawAnnotations, &secret.Annotations); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal secret %q: %w", name, err)
	}
	return secret, nil
}

// ListSecrets implements store.SecretStore.
func (s *Store) ListSecrets(ctx context.Context) ([]*v1.Secret, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, namespace, resource_version, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND namespace = $2`, kindSecret, defaultNamespace)
	if err != nil {
		return nil, fmt.Errorf("postgres: list secrets: %w", err)
	}
	defer rows.Close()

	return scanSecrets(rows)
}

// UpdateSecret implements store.SecretStore.
// Full-record replace used by the create-or-update PUT path (credential
// rotation). Increments resource_version and returns the new value.
func (s *Store) UpdateSecret(ctx context.Context, secret *v1.Secret) error {
	secret.ObjectMeta.UpdatedAt = time.Now().UTC()
	if secret.ObjectMeta.Namespace == "" {
		secret.ObjectMeta.Namespace = defaultNamespace
	}

	spec, err := json.Marshal(secret.Spec)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret spec: %w", err)
	}
	labels, err := json.Marshal(secret.Labels)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret labels: %w", err)
	}
	annotations, err := json.Marshal(secret.Annotations)
	if err != nil {
		return fmt.Errorf("postgres: marshal secret annotations: %w", err)
	}

	err = s.pool.QueryRow(ctx, `
		UPDATE resources
		SET spec = $1, labels = $2, annotations = $3, updated_at = $4,
		    resource_version = resource_version + 1
		WHERE kind = $5 AND namespace = $6 AND name = $7
		RETURNING resource_version`,
		spec, labels, annotations, secret.ObjectMeta.UpdatedAt,
		kindSecret, secret.ObjectMeta.Namespace, secret.Name,
	).Scan(&secret.ObjectMeta.ResourceVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: secret %q", store.ErrNotFound, secret.Name)
		}
		return fmt.Errorf("postgres: update secret %q: %w", secret.Name, err)
	}
	return nil
}

// DeleteSecret implements store.SecretStore.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM resources WHERE kind = $1 AND namespace = $2 AND name = $3`, kindSecret, defaultNamespace, name)
	if err != nil {
		return fmt.Errorf("postgres: delete secret %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: secret %q", store.ErrNotFound, name)
	}
	return nil
}

// ============================================================
// Helpers
// ============================================================

// scanSecrets iterates over query rows and decodes each secret.
func scanSecrets(rows pgx.Rows) ([]*v1.Secret, error) {
	var secrets []*v1.Secret
	for rows.Next() {
		var (
			name, namespace                               string
			resourceVersion                               int64
			rawSpec, rawStatus, rawLabels, rawAnnotations []byte
			createdAt, updatedAt                          time.Time
		)
		if err := rows.Scan(&name, &namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan secret row: %w", err)
		}
		secret := &v1.Secret{
			TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindSecret},
			ObjectMeta: v1.ObjectMeta{
				Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
				CreatedAt: createdAt, UpdatedAt: updatedAt,
			},
		}
		if err := unmarshalFields(name, rawSpec, &secret.Spec, rawStatus, &secret.Status, rawLabels, &secret.Labels, rawAnnotations, &secret.Annotations); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal secret %q: %w", name, err)
		}
		secrets = append(secrets, secret)
	}
	return secrets, rows.Err()
}

// scanProjects iterates over query rows and decodes each project.
func scanProjects(rows pgx.Rows) ([]*v1.Project, error) {
	var projects []*v1.Project
	for rows.Next() {
		var (
			name, namespace                               string
			resourceVersion                               int64
			rawSpec, rawStatus, rawLabels, rawAnnotations []byte
			createdAt, updatedAt                          time.Time
		)
		if err := rows.Scan(&name, &namespace, &resourceVersion, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan project row: %w", err)
		}
		project := &v1.Project{
			TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindProject},
			ObjectMeta: v1.ObjectMeta{
				Name: name, Namespace: namespace, ResourceVersion: resourceVersion,
				CreatedAt: createdAt, UpdatedAt: updatedAt,
			},
		}
		if err := unmarshalFields(name, rawSpec, &project.Spec, rawStatus, &project.Status, rawLabels, &project.Labels, rawAnnotations, &project.Annotations); err != nil {
			return nil, fmt.Errorf("postgres: unmarshal project %q: %w", name, err)
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

// unmarshalFields decodes the four JSONB columns into the provided pointers.
func unmarshalFields(name string, rawSpec []byte, spec any, rawStatus []byte, status any, rawLabels []byte, labels *map[string]string, rawAnnotations []byte, annotations *map[string]string) error {
	if err := json.Unmarshal(rawSpec, spec); err != nil {
		return fmt.Errorf("spec: %w", err)
	}
	if err := json.Unmarshal(rawStatus, status); err != nil {
		return fmt.Errorf("status: %w", err)
	}
	if len(rawLabels) > 0 && string(rawLabels) != "null" {
		if err := json.Unmarshal(rawLabels, labels); err != nil {
			return fmt.Errorf("labels: %w", err)
		}
	}
	if len(rawAnnotations) > 0 && string(rawAnnotations) != "null" {
		if err := json.Unmarshal(rawAnnotations, annotations); err != nil {
			return fmt.Errorf("annotations: %w", err)
		}
	}
	return nil
}

// isUniqueViolation returns true for PostgreSQL error code 23505 (unique_violation).
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// pgx wraps pgconn.PgError; check the SqlState code directly.
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) {
		return pgErr.SQLState() == "23505"
	}
	return false
}

// migrateLogger wraps zap.Logger to satisfy migrate.Logger.
type migrateLogger struct {
	logger *zap.Logger
}

func (l *migrateLogger) Printf(format string, v ...interface{}) {
	l.logger.Info(fmt.Sprintf(format, v...))
}

func (l *migrateLogger) Verbose() bool {
	return l.logger.Level() == zap.DebugLevel
}

// Compile-time assertion that *Store satisfies store.Store.
var _ store.Store = (*Store)(nil)
