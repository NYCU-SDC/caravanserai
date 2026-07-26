package agent

import (
	"context"
	"fmt"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
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

func (c conditionReporter) SetMaintenance(ctx context.Context, key backup.ResourceKey, reason string) error {
	return c.client.PatchProjectCondition(ctx, key.Name,
		v1.ConditionTypeMaintenance, v1.ConditionTrue, reason,
		"Containers are stopped for a Managed volume backup")
}

func (c conditionReporter) ClearMaintenance(ctx context.Context, key backup.ResourceKey) error {
	return c.client.ClearProjectCondition(ctx, key.Name, v1.ConditionTypeMaintenance)
}

// NewBackupSupport wires up Managed volume backups from the agent's config.
//
// It returns nil when no object store is configured, which disables backups
// entirely: Managed volumes are still provisioned and mounted, they are
// simply never uploaded. A Project that declares a backup policy while this
// agent has no object store is failed loudly by the runner rather than
// running silently unbacked.
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
		return nil, nil
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
	})
	if err != nil {
		return nil, fmt.Errorf("agent: create object store: %w", err)
	}

	coordinator := backup.NewCoordinator()
	runner := backup.NewRunner(
		coordinator,
		runtime,
		store,
		NewOwnershipResolver(client, cfg.NodeName, logger),
		conditionReporter{client: client},
		NewRouteRefresher(runtime, routes, logger),
		backup.Config{
			DataRoot:    cfg.DataRoot,
			NodeName:    cfg.NodeName,
			CaraVersion: caraVersion,
		},
		logger,
	)

	logger.Info("Managed volume backups enabled",
		zap.String("bucket", cfg.S3.Bucket),
		zap.String("endpoint", cfg.S3.Endpoint))

	return &BackupSupport{
		Supervisor:  backup.NewSupervisor(runner, logger),
		Coordinator: coordinator,
	}, nil
}
