package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// Run registers the node with the control-plane and then runs two concurrent
// loops until ctx is cancelled:
//
//  1. Heartbeat loop — sends a heartbeat every heartbeatInterval to keep the
//     node marked as Ready.
//
//  2. Project poll loop — every pollInterval, fetches Projects that have been
//     scheduled onto this node and reconciles them (runs workloads, reports
//     status back to the server).
//
// The initial registration is retried with a fixed 5-second back-off until it
// succeeds or ctx is cancelled, so that the agent can start before the server
// is ready.
func Run(ctx context.Context, client *Client, runtime docker.Runtime, heartbeatInterval time.Duration, agentPort int, logger *zap.Logger) {
	const pollInterval = 10 * time.Second

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
	bootstrapRunningProjects(ctx, client, runtime, logger)

	// ── Heartbeat loop ────────────────────────────────────────────────────
	heartbeatTicker := time.NewTicker(heartbeatInterval)
	defer heartbeatTicker.Stop()

	// ── Project poll loop ─────────────────────────────────────────────────
	pollTicker := time.NewTicker(pollInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case <-heartbeatTicker.C:
			status := v1.NodeStatus{
				State: v1.NodeStateReady,
				Network: v1.NodeNetworkStatus{
					AgentPort: agentPort,
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
			reconcileProjects(ctx, client, runtime, logger)
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
// awareness after a restart.
func bootstrapRunningProjects(ctx context.Context, client *Client, runtime docker.Runtime, logger *zap.Logger) {
	projects, err := client.ListProjectsForReconcile(ctx)
	if err != nil {
		logger.Warn("Bootstrap: failed to list projects", zap.Error(err))
		return
	}

	var running int
	for _, p := range projects {
		if p.Status.Phase == v1.ProjectPhaseRunning {
			running++
			healthCheckOne(ctx, client, runtime, p, logger)
		}
	}

	logger.Info("Bootstrap: found running projects on this node", zap.Int("count", running))
}

// reconcileProjects fetches all Scheduled, Running, and Terminating Projects
// assigned to this node and processes each one:
//   - Terminating → tear down containers
//   - Running → health-check containers
//   - Scheduled → reconcile (create/start) containers
func reconcileProjects(ctx context.Context, client *Client, runtime docker.Runtime, logger *zap.Logger) {
	projects, err := client.ListProjectsForReconcile(ctx)
	if err != nil {
		logger.Warn("Failed to list projects for reconcile", zap.Error(err))
		return
	}

	if len(projects) == 0 {
		return
	}

	logger.Info("Reconciling projects", zap.Int("count", len(projects)))

	for _, p := range projects {
		switch p.Status.Phase {
		case v1.ProjectPhaseTerminating:
			terminateOne(ctx, client, runtime, p, logger)
		case v1.ProjectPhaseRunning:
			healthCheckOne(ctx, client, runtime, p, logger)
		default:
			reconcileOne(ctx, client, runtime, p, logger)
		}
	}
}

// reconcileOne reconciles a single project:
//  1. Inspect current container states.
//  2. If any container exited with a non-zero code → report Failed.
//  3. If all containers are running and count matches → report Running.
//  4. Otherwise call ReconcileProject to create/start missing containers, then
//     report Running on success or Failed on error.
func reconcileOne(ctx context.Context, client *Client, runtime docker.Runtime, p *v1.Project, logger *zap.Logger) {
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
		return
	}

	// Some containers are missing or not running — reconcile.
	log.Info("Reconciling project containers",
		zap.Int("running", runningCount),
		zap.Int("expected", len(p.Spec.Services)),
	)
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
}

// terminateOne tears down all Docker resources for a Terminating project and
// reports Terminated back to the server.  The ProjectTerminationController on
// the server will then perform the final store deletion.
func terminateOne(ctx context.Context, client *Client, runtime docker.Runtime, p *v1.Project, logger *zap.Logger) {
	log := logger.With(zap.String("project", p.Name))
	log.Info("Removing Docker resources for Terminating project")

	if err := runtime.RemoveProject(ctx, p.Name, p.Spec); err != nil {
		log.Error("Failed to remove project resources", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name,
			v1.ProjectPhaseFailed,
			"RemoveError",
			err.Error(),
		)
		return
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
func healthCheckOne(ctx context.Context, client *Client, runtime docker.Runtime, p *v1.Project, logger *zap.Logger) {
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

	// All containers are running — healthy, nothing to do.
	log.Debug("All containers healthy, nothing to do")
}
