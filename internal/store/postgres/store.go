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
		INSERT INTO resources (kind, name, phase, spec, status, labels, annotations, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		kindNode, node.Name, string(node.Status.State),
		spec, status, labels, annotations,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: create node %q: %w", node.Name, err)
	}
	s.publish(event.TopicNodeCreated, node.Name)
	return nil
}

// GetNode implements store.NodeStore.
func (s *Store) GetNode(ctx context.Context, name string) (*v1.Node, error) {
	return s.getNode(ctx, name)
}

func (s *Store) getNode(ctx context.Context, name string) (*v1.Node, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND name = $2`,
		kindNode, name,
	)

	var (
		rawSpec, rawStatus, rawLabels, rawAnnotations []byte
		createdAt, updatedAt                          time.Time
	)
	if err := row.Scan(&rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get node %q: %w", name, err)
	}

	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindNode},
		ObjectMeta: v1.ObjectMeta{Name: name, CreatedAt: createdAt, UpdatedAt: updatedAt},
	}
	if err := unmarshalFields(name, rawSpec, &node.Spec, rawStatus, &node.Status, rawLabels, &node.Labels, rawAnnotations, &node.Annotations); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal node %q: %w", name, err)
	}
	return node, nil
}

// ListNodes implements store.NodeStore.
func (s *Store) ListNodes(ctx context.Context) ([]*v1.Node, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1`, kindNode)
	if err != nil {
		return nil, fmt.Errorf("postgres: list nodes: %w", err)
	}
	defer rows.Close()

	var nodes []*v1.Node
	for rows.Next() {
		var (
			name                                          string
			rawSpec, rawStatus, rawLabels, rawAnnotations []byte
			createdAt, updatedAt                          time.Time
		)
		if err := rows.Scan(&name, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan node row: %w", err)
		}
		node := &v1.Node{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindNode},
			ObjectMeta: v1.ObjectMeta{Name: name, CreatedAt: createdAt, UpdatedAt: updatedAt},
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

	tag, err := s.pool.Exec(ctx, `
		UPDATE resources
		SET phase = $1, spec = $2, status = $3, labels = $4, annotations = $5, updated_at = $6
		WHERE kind = $7 AND name = $8`,
		string(node.Status.State), spec, status, labels, annotations,
		node.ObjectMeta.UpdatedAt, kindNode, node.Name,
	)
	if err != nil {
		return fmt.Errorf("postgres: update node %q: %w", node.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: node %q", store.ErrNotFound, node.Name)
	}
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
func (s *Store) UpdateNodeStatus(ctx context.Context, name string, status v1.NodeStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("postgres: marshal node status: %w", err)
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE resources
		SET phase = $1, status = $2, updated_at = $3
		WHERE kind = $4 AND name = $5`,
		string(status.State), raw, time.Now().UTC(), kindNode, name,
	)
	if err != nil {
		return fmt.Errorf("postgres: update node status %q: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: node %q", store.ErrNotFound, name)
	}
	s.publish(event.TopicNodeUpdated, name)
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
		INSERT INTO resources (kind, name, phase, spec, status, labels, annotations, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		kindProject, project.Name, string(project.Status.Phase),
		spec, status, labels, annotations,
		now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return store.ErrAlreadyExists
		}
		return fmt.Errorf("postgres: create project %q: %w", project.Name, err)
	}
	s.publish(event.TopicProjectCreated, project.Name)
	return nil
}

// GetProject implements store.ProjectStore.
func (s *Store) GetProject(ctx context.Context, name string) (*v1.Project, error) {
	return s.getProject(ctx, name)
}

func (s *Store) getProject(ctx context.Context, name string) (*v1.Project, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND name = $2`,
		kindProject, name,
	)

	var (
		rawSpec, rawStatus, rawLabels, rawAnnotations []byte
		createdAt, updatedAt                          time.Time
	)
	if err := row.Scan(&rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("postgres: get project %q: %w", name, err)
	}

	project := &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindProject},
		ObjectMeta: v1.ObjectMeta{Name: name, CreatedAt: createdAt, UpdatedAt: updatedAt},
	}
	if err := unmarshalFields(name, rawSpec, &project.Spec, rawStatus, &project.Status, rawLabels, &project.Labels, rawAnnotations, &project.Annotations); err != nil {
		return nil, fmt.Errorf("postgres: unmarshal project %q: %w", name, err)
	}
	return project, nil
}

// ListProjects implements store.ProjectStore.
func (s *Store) ListProjects(ctx context.Context) ([]*v1.Project, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT name, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1`, kindProject)
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
		SELECT name, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND phase = $2`,
		kindProject, string(phase),
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
		SELECT name, spec, status, labels, annotations, created_at, updated_at
		FROM resources WHERE kind = $1 AND phase = ANY($2)`,
		kindProject, phaseStrings,
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
		SELECT name, spec, status, labels, annotations, created_at, updated_at
		FROM resources
		WHERE kind = $1 AND phase = ANY($2) AND status->>'nodeRef' = $3`,
		kindProject, phaseStrings, nodeRef,
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

	tag, err := s.pool.Exec(ctx, `
		UPDATE resources
		SET phase = $1, spec = $2, status = $3, labels = $4, annotations = $5, updated_at = $6
		WHERE kind = $7 AND name = $8`,
		string(project.Status.Phase), spec, status, labels, annotations,
		project.ObjectMeta.UpdatedAt, kindProject, project.Name,
	)
	if err != nil {
		return fmt.Errorf("postgres: update project %q: %w", project.Name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: project %q", store.ErrNotFound, project.Name)
	}
	return nil
}

// DeleteProject implements store.ProjectStore.
func (s *Store) DeleteProject(ctx context.Context, name string) error {
	tag, err := s.pool.Exec(ctx,
		`DELETE FROM resources WHERE kind = $1 AND name = $2`, kindProject, name)
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
		SET phase = $1, status = $2, updated_at = $3
		WHERE kind = $4 AND name = $5`,
		string(status.Phase), raw, time.Now().UTC(), kindProject, name,
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

// ============================================================
// Helpers
// ============================================================

// scanProjects iterates over query rows and decodes each project.
func scanProjects(rows pgx.Rows) ([]*v1.Project, error) {
	var projects []*v1.Project
	for rows.Next() {
		var (
			name                                          string
			rawSpec, rawStatus, rawLabels, rawAnnotations []byte
			createdAt, updatedAt                          time.Time
		)
		if err := rows.Scan(&name, &rawSpec, &rawStatus, &rawLabels, &rawAnnotations, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan project row: %w", err)
		}
		project := &v1.Project{
			TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: kindProject},
			ObjectMeta: v1.ObjectMeta{Name: name, CreatedAt: createdAt, UpdatedAt: updatedAt},
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
