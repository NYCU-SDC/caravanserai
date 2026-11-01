package agent

import (
	"context"
	"fmt"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/pgbackup"
	"NYCU-SDC/caravanserai/internal/agent/restore"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/objectstore"

	"go.uber.org/zap"
)

// conditionReporter publishes the Maintenance condition so a backup is
// visible from the control plane.
//
// This is observability, not a lock. What actually keeps a backing-up Project
// from being misreported is the agent-local coordinator, which the poll loop
// consults directly; a condition written over the network can fail or arrive
// late, so correctness never rests on it. Accordingly the Runner logs and
// continues when these calls fail.
type conditionReporter struct {
	client *Client
}

func (c conditionReporter) SetMaintenance(ctx context.Context, key backup.ResourceKey, uid, nodeRef string, generation int64, reason string) error {
	fence := AssignmentFence{UID: uid, NodeRef: nodeRef, Generation: generation}
	return c.client.PatchProjectCondition(ctx, key.Name, fence,
		v1.ConditionTypeMaintenance, v1.ConditionTrue, reason,
		"Containers are stopped for a Managed volume backup")
}

func (c conditionReporter) ClearMaintenance(ctx context.Context, key backup.ResourceKey, uid, nodeRef string, generation int64) error {
	fence := AssignmentFence{UID: uid, NodeRef: nodeRef, Generation: generation}
	return c.client.ClearProjectCondition(ctx, key.Name, fence, v1.ConditionTypeMaintenance)
}

func (c conditionReporter) SetBackupFailing(ctx context.Context, project *v1.Project) error {
	return c.client.PatchProjectCondition(ctx, project.Name, fenceForProject(project),
		v1.ConditionTypeBackupFailing, v1.ConditionTrue, v1.ReasonBackupAttemptFailed,
		"The most recent scheduled backup failed; see this Node's Agent log")
}

func (c conditionReporter) ClearBackupFailing(ctx context.Context, project *v1.Project) error {
	return c.client.ClearProjectCondition(ctx, project.Name, fenceForProject(project), v1.ConditionTypeBackupFailing)
}

// backupCommitter adapts Client to backup.BackupCommitter.
type backupCommitter struct {
	client *Client
}

func (c backupCommitter) CommitBackup(ctx context.Context, key backup.ResourceKey, uid, nodeRef string, generation int64, backupID string) error {
	return c.client.CommitBackup(ctx, key.Name, AssignmentFence{UID: uid, NodeRef: nodeRef, Generation: generation}, backupID)
}

// restoreTimeout bounds a single Project's restore. Generous, because it
// covers downloading and extracting every Managed volume the Project owns over
// whatever link the node has; the point is only to stop a stalled transfer from
// pinning the Project's coordinator slot forever.
const restoreTimeout = 30 * time.Minute

// NewBackupSupport wires up Managed volume backups from the agent's config.
//
// Without an object store configured, backups are disabled entirely: there is
// no Supervisor, Managed volumes are still provisioned and mounted, they are
// simply never uploaded or restored. Support is still returned, without an
// object store, so placement can decide PostgresDifferential Projects from
// local provenance on every agent (see ensureVolumeData).
func NewBackupSupport(
	cfg config.AgentConfig,
	client *Client,
	runtime docker.Runtime,
	routes RouteUpdater,
	caraVersion string,
	logger *zap.Logger,
) (*BackupSupport, error) {
	if !cfg.S3.Enabled() {
		logger.Info("Object store not configured; Managed volume backups are disabled")
		return &BackupSupport{
			Coordinator: backup.NewCoordinator(),
			// No store: only the local operations (initialising empty
			// volumes, writing provenance) are reachable without one.
			Restorer: restore.NewRestorer(nil, restore.Config{
				DataRoot: cfg.DataRoot,
				NodeName: cfg.NodeName,
			}, logger),
			DataRoot: cfg.DataRoot,
			NodeName: cfg.NodeName,
		}, nil
	}

	host, secure, err := objectstore.ParseEndpoint(cfg.S3.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("agent: object store endpoint: %w", err)
	}

	store, err := objectstore.NewMinioStore(objectstore.MinioConfig{
		Endpoint:  host,
		Bucket:    cfg.S3.Bucket,
		Region:    cfg.S3.Region,
		Secure:    secure,
		AccessKey: cfg.S3.AccessKey,
		SecretKey: cfg.S3.SecretKey,
		CAFile:    cfg.S3.CAFile,
	})
	if err != nil {
		return nil, fmt.Errorf("agent: create object store: %w", err)
	}

	// Anything in staging now belongs to a run whose process is gone — this
	// agent has not started one yet. Sweep it before any backup can trip the
	// free-space precondition on space that is already dead.
	stagingDir := backup.DefaultStagingDir(cfg.DataRoot)
	if err := backup.CleanStaging(stagingDir); err != nil {
		logger.Warn("Failed to clean stale backup staging", zap.String("path", stagingDir), zap.Error(err))
	}

	coordinator := backup.NewCoordinator()
	runner := backup.NewRunner(
		coordinator,
		runtime,
		store,
		NewOwnershipResolver(client, cfg.NodeName, logger),
		conditionReporter{client: client},
		backupCommitter{client: client},
		NewRouteRefresher(runtime, routes, logger),
		backup.Config{
			DataRoot:     cfg.DataRoot,
			NodeName:     cfg.NodeName,
			CaraVersion:  caraVersion,
			MinFreeBytes: cfg.MinFreeBytes,
		},
		logger,
	)

	logger.Info("Managed volume backups enabled",
		zap.String("bucket", cfg.S3.Bucket),
		zap.String("endpoint", cfg.S3.Endpoint))

	// The restorer reads from the same bucket the runner writes to — a
	// generation is only restorable from where it was uploaded.
	restorer := restore.NewRestorer(store, restore.Config{
		DataRoot:     cfg.DataRoot,
		MinFreeBytes: cfg.MinFreeBytes,
		Timeout:      restoreTimeout,
		NodeName:     cfg.NodeName,
	}, logger)

	strategies := strategyRunner{offline: runner}
	if rt, ok := runtime.(pgRuntimeWithLogs); ok {
		continuity := pgbackup.NewFileContinuity(cfg.DataRoot, rt)
		strategies.postgres = pgbackup.NewRunner(coordinator, rt, continuity,
			newCompletionRecorder(client, store, caraVersion), logger)
	}

	return &BackupSupport{
		Supervisor:  backup.NewSupervisor(strategies, logger).WithHealthReporter(conditionReporter{client: client}),
		Coordinator: coordinator,
		Restorer:    restorer,
		DataRoot:    cfg.DataRoot,
		NodeName:    cfg.NodeName,
		ObjectStore: true,
	}, nil
}

// pgRuntimeWithLogs is what PostgreSQL backups need from the runtime: running
// commands in the database container and reading its log.
type pgRuntimeWithLogs interface {
	pgbackup.Runtime
	pgbackup.LogReader
}

// strategyRunner sends each Project's backup to the runner of its strategy. A
// Project uses one strategy for every Managed volume, which validation
// enforces.
type strategyRunner struct {
	offline  *backup.Runner
	postgres *pgbackup.Runner
}

func (s strategyRunner) Run(ctx context.Context, project *v1.Project) error {
	if project.Spec.HasPostgresDifferential() {
		if s.postgres == nil {
			return fmt.Errorf("agent: this Node's container runtime cannot run PostgreSQL backups for %s", project.Name)
		}
		return s.postgres.Run(ctx, project)
	}
	return s.offline.Run(ctx, project)
}
