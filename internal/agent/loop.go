package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"go.uber.org/zap"
)

// busyChecker reports whether a Project has an agent-local operation in
// flight, and lets a caller claim one of its own. Satisfied by
// *backup.Coordinator.
//
// terminateOne claims OpTerminate before tearing down a Project's Docker
// resources so a backup supervisor tick cannot start mid-teardown: the
// Coordinator only protects against overlap between operations that actually
// claim it, and until terminate claimed too, a backup could start while
// containers were being removed out from under it.
type busyChecker interface {
	IsBusy(key backup.ResourceKey) bool
	TryClaim(key backup.ResourceKey, op backup.Operation) (release func(), ok bool)
}

// BackupSupport bundles the optional Managed volume data wiring. It is nil
// when the agent has no object store configured, in which case the poll loop
// behaves exactly as it did before backups existed.
//
// The fields travel together by necessity: the Supervisor decides when a
// backup runs, the Restorer puts volumes back before containers start, and the
// Coordinator is how each learns the other is working on the same Project.
type BackupSupport struct {
	Supervisor  *backup.Supervisor
	Coordinator *backup.Coordinator
	Restorer    *restore.Restorer
	// DataRoot is where Managed volume data and restore markers live.
	DataRoot string
}

// RouteUpdater is the narrow interface consumed by the agent loop to maintain
// proxy routes.  It is satisfied by *proxy.RouteTable.
type RouteUpdater interface {
	// Update builds routes from the project's ingress definitions and the
	// discovered container IPs.  Existing routes for the project are replaced.
	Update(project *v1.Project, containerIPs map[string]string)

	// Remove deletes all routes belonging to the named project.
	Remove(projectName string)
}

// RunConfig configures Run. Client, Runtime, HeartbeatInterval, and Logger are
// required; Routes and Backups are optional.
type RunConfig struct {
	Client            *Client
	Runtime           docker.Runtime
	HeartbeatInterval time.Duration
	AgentPort         int
	AdvertiseIP       string

	// Routes maintains proxy routes for projects with ingress definitions.
	// Nil disables proxy route maintenance.
	Routes RouteUpdater

	// Backups schedules Managed volume backups per Project and makes the poll
	// loop skip Projects with an operation in flight. Nil disables backups.
	Backups *BackupSupport

	Logger *zap.Logger
}

// Run registers the node with the control-plane and then runs two concurrent
// loops until ctx is cancelled:
//
//  1. Heartbeat loop — sends a heartbeat every cfg.HeartbeatInterval to keep
//     the node marked as Ready.
//
//  2. Project poll loop — every pollInterval, fetches Projects that have been
//     scheduled onto this node and reconciles them (runs workloads, reports
//     status back to the server).
//
// The initial registration is retried with a fixed 5-second back-off until it
// succeeds or ctx is cancelled, so that the agent can start before the server
// is ready.
func Run(ctx context.Context, cfg RunConfig) {
	const pollInterval = 10 * time.Second

	client, runtime, routes, logger := cfg.Client, cfg.Runtime, cfg.Routes, cfg.Logger

	if cfg.Backups != nil {
		if cfg.Backups.Supervisor != nil {
			defer cfg.Backups.Supervisor.Stop()
		}

		// Sweep the leftovers of a restore that died mid-swap. Done once, before
		// any reconcile, so the space is reclaimed before a fresh restore asks
		// for it. Deliberately not fatal: leftovers waste disk but corrupt
		// nothing, and refusing to start the agent over wasted disk is worse
		// than running with it.
		if err := restore.CleanDisplaced(cfg.Backups.DataRoot); err != nil {
			logger.Warn("Failed to clean displaced volume data", zap.Error(err))
		}
	}

	spec := v1.NodeSpec{
		Hostname: client.nodeName,
	}

	// ── Registration (with retry) ──────────────────────────────────────────
	for {
		if err := client.Register(ctx, spec); err != nil {
			logger.Warn("Node registration failed, retrying in 5s", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
				continue
			}
		}
		break
	}

	// ── Bootstrap: health-check Running projects ──────────────────────────
	// After a restart, the Agent has no memory of Running projects. Fetch
	// them from the server and verify containers are still alive so that
	// failures are detected immediately rather than waiting for the first
	// poll tick.
	bootstrapRunningProjects(ctx, client, runtime, routes, logger)

	// ── Heartbeat loop ────────────────────────────────────────────────────
	heartbeatTicker := time.NewTicker(cfg.HeartbeatInterval)
	defer heartbeatTicker.Stop()

	// ── Project poll loop ─────────────────────────────────────────────────
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	// Orphan sweep state lives across ticks: a project must be observed absent
	// for the whole grace period, which spans many polls.
	orphans := newOrphanTracker(realClock{})

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeatTicker.C:
			status := v1.NodeStatus{
				State: v1.NodeStateReady,
				Network: v1.NodeNetworkStatus{
					IP:        cfg.AdvertiseIP,
					AgentPort: cfg.AgentPort,
				},
			}
			if err := client.Heartbeat(ctx, status); err != nil {
				if errors.Is(err, ErrNodeNotFound) {
					logger.Info("Node not found on server (404), initiating re-registration")
					if err := reRegister(ctx, client, spec, logger); err != nil {
						return // context cancelled
					}
				} else {
					logger.Warn("Heartbeat failed", zap.Error(err))
				}
			}

		case <-pollTicker.C:
			reconcileProjects(ctx, client, runtime, routes, cfg.Backups, orphans, logger)
		}
	}
}

// reRegister attempts to re-register the node with exponential backoff.
// It starts at 5s and doubles up to a 60s cap. Returns nil on success or a
// non-nil error only when ctx is cancelled.
func reRegister(ctx context.Context, client *Client, spec v1.NodeSpec, logger *zap.Logger) error {
	const (
		initialBackoff = 5 * time.Second
		maxBackoff     = 60 * time.Second
	)
	backoff := initialBackoff

	for {
		if err := client.Register(ctx, spec); err != nil {
			logger.Warn("Re-registration failed, retrying",
				zap.Error(err),
				zap.Duration("backoff", backoff),
			)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
		}

		logger.Info("Node re-registered successfully")
		return nil
	}
}

// bootstrapRunningProjects fetches all projects (including Running) from the
// server and runs healthCheckOne on each Running project to rebuild the Agent's
// awareness after a restart.  For healthy Running projects with ingress rules,
// proxy routes are re-established.
func bootstrapRunningProjects(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, logger *zap.Logger) {
	projects, err := client.ListProjectsForReconcile(ctx)
	if err != nil {
		logger.Warn("Bootstrap: failed to list projects", zap.Error(err))
		return
	}

	var running int
	for _, p := range projects {
		if p.Status.Phase == v1.ProjectPhaseRunning {
			running++
			healthCheckOne(ctx, client, runtime, routes, p, logger)
		}
	}

	logger.Info("Bootstrap: found running projects on this node", zap.Int("count", running))
}

// reconcileProjects fetches all Scheduled, Running, and Terminating Projects
// assigned to this node and processes each one:
//   - Terminating → tear down containers
//   - Running → health-check containers
//   - Scheduled → reconcile (create/start) containers
//
// backups may be nil when the agent has no object store configured. When
// present it is consulted three times: to skip Projects with an agent-local
// operation in flight, to keep the per-Project backup goroutines in step with
// the Projects this node currently holds, and to put Managed volume data in
// place before a Project's containers are created.
//
// orphans carries the sweep's cross-tick state. It may be nil, which disables
// the sweep.
func reconcileProjects(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, backups *BackupSupport, orphans *orphanTracker, logger *zap.Logger) {
	projects, err := client.ListProjectsForReconcile(ctx)
	if err != nil {
		// No sweep this tick, and no clock advanced: the agent cannot tell an
		// unreachable server from a project that genuinely moved away.
		logger.Warn("Failed to list projects for reconcile", zap.Error(err))
		return
	}

	// Sweep before the early return below: a node that lost every project still
	// has orphans to clean up, and that is exactly the case an empty list
	// describes.
	if orphans != nil {
		sweepOrphans(ctx, runtime, routes, orphans, projects, logger)
	}

	var busy busyChecker
	if backups != nil && backups.Coordinator != nil {
		busy = backups.Coordinator
	}

	// Sync before the early return: an empty list means every Project left
	// this node, and their backup goroutines must be cancelled.
	if backups != nil && backups.Supervisor != nil {
		backups.Supervisor.Sync(ctx, projects)
	}

	if len(projects) == 0 {
		return
	}

	logger.Info("Reconciling projects", zap.Int("count", len(projects)))

	for _, p := range projects {
		// A Project whose containers are deliberately stopped — for a backup,
		// a restore, a terminate — must be skipped entirely for this tick: no
		// health check, no reconcile, no status write. Without this the
		// health check would see missing containers and report Failed, which
		// both misreports a healthy service and unlocks apply/delete paths
		// that are meant to be closed while the Project is Running.
		if busy != nil {
			key := backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}
			if busy.IsBusy(key) {
				logger.Debug("Skipping project with an operation in flight",
					zap.String("project", key.String()))
				continue
			}
		}

		switch p.Status.Phase {
		case v1.ProjectPhaseTerminating:
			terminateOne(ctx, client, runtime, routes, busy, p, logger)
		case v1.ProjectPhaseRunning:
			healthCheckOne(ctx, client, runtime, routes, p, logger)
		default:
			reconcileOne(ctx, client, runtime, routes, backups, p, logger)
		}
	}
}

// reconcileOne reconciles a single project:
//  1. Inspect current container states.
//  2. If any container exited with a non-zero code → report Failed.
//  3. If all containers are running and count matches → report Running.
//  4. Otherwise call ReconcileProject to create/start missing containers, then
//     report Running on success or Failed on error.
//
// After a successful transition to Running, proxy routes are updated.
func reconcileOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, backups *BackupSupport, p *v1.Project, logger *zap.Logger) {
	log := logger.With(zap.String("project", p.Name))

	states, err := runtime.InspectProject(ctx, p)
	if err != nil {
		log.Warn("Failed to inspect project containers", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name,
			v1.ProjectPhaseFailed,
			"InspectError",
			err.Error(),
		)
		return
	}

	// Check for containers that exited with a non-zero exit code.
	var failedSvcs []string
	for _, s := range states {
		if s.Status == "exited" && s.ExitCode != 0 {
			failedSvcs = append(failedSvcs, fmt.Sprintf("%s(exit=%d)", s.ServiceName, s.ExitCode))
		}
	}
	if len(failedSvcs) > 0 {
		msg := "Containers exited with errors: " + strings.Join(failedSvcs, ", ")
		log.Warn("Project has failed containers", zap.String("detail", msg))
		_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "ContainerExited", msg)
		return
	}

	// Check whether every service already has a running container.
	runningCount := 0
	for _, s := range states {
		if s.Status == "running" {
			runningCount++
		}
	}
	if runningCount == len(p.Spec.Services) && len(p.Spec.Services) > 0 {
		log.Debug("All containers running, nothing to do")
		if err := client.UpdateProjectStatus(ctx, p.Name,
			v1.ProjectPhaseRunning,
			"ContainersRunning",
			"All containers running",
		); err != nil {
			log.Warn("Failed to update project status", zap.Error(err))
		}
		updateProxyRoutes(ctx, runtime, routes, p, log)
		return
	}

	// Some containers are missing or not running — reconcile.
	log.Info("Reconciling project containers",
		zap.Int("running", runningCount),
		zap.Int("expected", len(p.Spec.Services)),
	)

	// Managed volume data must be in place before any container can mount it.
	// Reaching here means containers are about to be created or started, which
	// is the last moment the volumes can be populated without a service seeing
	// an empty directory.
	if backups != nil {
		err := ensureVolumeData(ctx, backups.Restorer, backups.Coordinator, backups.DataRoot, p, logger)
		switch {
		case err == nil:
		case errors.Is(err, errDeferred):
			// Another operation holds the Project. Leave its status untouched
			// and let the next tick try again — losing a race is not a fault.
			return
		default:
			log.Error("Failed to prepare volume data", zap.Error(err))
			_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "RestoreError", err.Error())
			return
		}
	}

	if err := runtime.ReconcileProject(ctx, p); err != nil {
		log.Error("Failed to reconcile project", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "ReconcileError", err.Error())
		return
	}

	if err := client.UpdateProjectStatus(ctx, p.Name,
		v1.ProjectPhaseRunning,
		"ContainersRunning",
		"All containers running",
	); err != nil {
		log.Warn("Failed to update project status to Running", zap.Error(err))
	}

	updateProxyRoutes(ctx, runtime, routes, p, log)
}

// terminateOne tears down all Docker resources for a Terminating project and
// reports Terminated back to the server.  The ProjectTerminationController on
// the server will then perform the final store deletion.
//
// Proxy routes are removed after successful teardown.
func terminateOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, busy busyChecker, p *v1.Project, logger *zap.Logger) {
	log := logger.With(zap.String("project", p.Name))

	// Claim the Project before touching Docker so a backup supervisor tick
	// cannot start (or continue) mid-teardown. reconcileProjects already
	// checked IsBusy before dispatching here, but that check and this claim
	// are not atomic — a backup goroutine can win the race in between, so the
	// claim can still legitimately fail. Skip this tick rather than tear down
	// half of what a concurrent backup is reading; the next poll retries.
	if busy != nil {
		key := backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}
		release, ok := busy.TryClaim(key, backup.OpTerminate)
		if !ok {
			log.Debug("Deferring termination: project busy with another operation",
				zap.String("project", key.String()))
			return
		}
		defer release()
	}

	log.Info("Removing Docker resources for Terminating project")

	if err := runtime.RemoveProject(ctx, p.Namespace, p.Name, p.Spec); err != nil {
		log.Error("Failed to remove project resources", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name,
			v1.ProjectPhaseFailed,
			"RemoveError",
			err.Error(),
		)
		return
	}

	if routes != nil {
		routes.Remove(p.Name)
		log.Info("Removed proxy routes for project")
	}

	log.Info("Project resources removed, reporting Terminated")
	if err := client.UpdateProjectStatus(ctx, p.Name,
		v1.ProjectPhaseTerminated,
		"ResourcesRemoved",
		"All Docker resources have been removed",
	); err != nil {
		log.Warn("Failed to update project status to Terminated", zap.Error(err))
	}
}

// healthCheckOne inspects a Running project's containers and reports Failed if
// any container has crashed, exited, or is missing.  It does NOT attempt to
// restart containers — it only reports the observed state.  A future
// ProjectRecoveryController will handle automated recovery.
//
// For healthy projects, proxy routes are re-affirmed to handle container IP
// changes after a restart.
func healthCheckOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, p *v1.Project, logger *zap.Logger) {
	log := logger.With(zap.String("project", p.Name))

	states, err := runtime.InspectProject(ctx, p)
	if err != nil {
		log.Warn("Failed to inspect project containers", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name,
			v1.ProjectPhaseFailed,
			"InspectError",
			err.Error(),
		)
		return
	}

	// Check for containers that exited with a non-zero exit code (crash).
	var crashedSvcs []string
	for _, s := range states {
		if s.Status == "exited" && s.ExitCode != 0 {
			crashedSvcs = append(crashedSvcs, fmt.Sprintf("%s(exit=%d)", s.ServiceName, s.ExitCode))
		}
	}
	if len(crashedSvcs) > 0 {
		msg := "Containers crashed: " + strings.Join(crashedSvcs, ", ")
		log.Warn("Project has crashed containers", zap.String("detail", msg))
		_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "ContainerCrashed", msg)
		return
	}

	// Check for missing containers (fewer than expected).
	if len(states) < len(p.Spec.Services) {
		// Build the list of services that have containers.
		have := make(map[string]bool, len(states))
		for _, s := range states {
			have[s.ServiceName] = true
		}
		var missingSvcs []string
		for _, svc := range p.Spec.Services {
			if !have[svc.Name] {
				missingSvcs = append(missingSvcs, svc.Name)
			}
		}
		msg := fmt.Sprintf("Missing containers for services: %s (expected %d, found %d)",
			strings.Join(missingSvcs, ", "), len(p.Spec.Services), len(states))
		log.Warn("Project has missing containers", zap.String("detail", msg))
		_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "ContainerMissing", msg)
		return
	}

	// Check for containers that exited cleanly (exit code 0) — ambiguous but
	// treated as Failed for safety. The user can investigate.
	var exitedSvcs []string
	for _, s := range states {
		if s.Status == "exited" && s.ExitCode == 0 {
			exitedSvcs = append(exitedSvcs, fmt.Sprintf("%s(exit=0)", s.ServiceName))
		}
	}
	if len(exitedSvcs) > 0 {
		msg := "Containers exited cleanly: " + strings.Join(exitedSvcs, ", ")
		log.Warn("Project has exited containers", zap.String("detail", msg))
		_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "ContainerExited", msg)
		return
	}

	// All containers are running — healthy.
	// Re-affirm proxy routes to handle container IP changes after restart.
	log.Debug("All containers healthy, nothing to do")
	updateProxyRoutes(ctx, runtime, routes, p, log)
}

// updateProxyRoutes discovers container IPs and updates the proxy route table
// for a project.  It is a no-op if routes is nil or the project has no ingress
// definitions.
func updateProxyRoutes(ctx context.Context, runtime docker.Runtime, routes RouteUpdater, p *v1.Project, log *zap.Logger) {
	if routes == nil || len(p.Spec.Ingress) == 0 {
		return
	}

	ips, err := runtime.GetContainerIPs(ctx, p)
	if err != nil {
		log.Warn("Failed to get container IPs for proxy routes", zap.Error(err))
		return
	}

	routes.Update(p, ips)
	log.Info("Updated proxy routes for project")
}
