package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// errSecretKeyNotFound is returned by resolveSecrets when a referenced Secret
// exists but does not contain the requested key. Like ErrSecretNotFound this
// is terminal for the project (Failed, no retry at this layer).
var errSecretKeyNotFound = errors.New("secret key not found")

// RouteUpdater is the narrow interface consumed by the agent loop to maintain
// proxy routes.  It is satisfied by *proxy.RouteTable.
type RouteUpdater interface {
	// Update builds routes from the project's ingress definitions and the
	// discovered container IPs.  Existing routes for the project are replaced.
	Update(project *v1.Project, containerIPs map[string]string)

	// Remove deletes all routes belonging to the named project.
	Remove(projectName string)
}

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
//
// If routes is non-nil, the agent will maintain proxy routes for projects that
// have ingress definitions.
func Run(ctx context.Context, client *Client, runtime docker.Runtime, heartbeatInterval time.Duration, agentPort int, advertiseIP string, routes RouteUpdater, logger *zap.Logger) {
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
	bootstrapRunningProjects(ctx, client, runtime, routes, logger)

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
					IP:        advertiseIP,
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
			reconcileProjects(ctx, client, runtime, routes, logger)
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
func reconcileProjects(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, logger *zap.Logger) {
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
			terminateOne(ctx, client, runtime, routes, p, logger)
		case v1.ProjectPhaseRunning:
			healthCheckOne(ctx, client, runtime, routes, p, logger)
		default:
			reconcileOne(ctx, client, runtime, routes, p, logger)
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
func reconcileOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, p *v1.Project, logger *zap.Logger) {
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

	// Some containers are missing or not running — resolve secret references,
	// then reconcile. The runtime only ever sees the resolved copy.
	resolved, err := resolveSecrets(ctx, client, p)
	if err != nil {
		switch {
		case errors.Is(err, ErrSecretNotFound):
			log.Warn("Referenced secret does not exist", zap.Error(err))
			_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "SecretNotFound", err.Error())
		case errors.Is(err, errSecretKeyNotFound):
			log.Warn("Referenced secret key does not exist", zap.Error(err))
			_ = client.UpdateProjectStatus(ctx, p.Name, v1.ProjectPhaseFailed, "SecretKeyNotFound", err.Error())
		default:
			// Transient fetch failure (network, 5xx): do not fail the
			// project — skip this tick and retry on the next poll.
			log.Warn("Failed to fetch referenced secrets, will retry next poll", zap.Error(err))
		}
		return
	}

	log.Info("Reconciling project containers",
		zap.Int("running", runningCount),
		zap.Int("expected", len(p.Spec.Services)),
	)
	if err := runtime.ReconcileProject(ctx, resolved); err != nil {
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

// resolveSecrets replaces every EnvVar.valueFrom.secretKeyRef in the project
// with the referenced Secret's literal value, so the container runtime only
// ever receives plain KEY=VALUE pairs (the kubelet pattern from CARA-57).
//
// It operates on a deep copy — the caller's project keeps its secretKeyRef
// references and never holds plaintext values. Resolved values live only in
// the returned copy, in memory; they must never be logged or written to disk.
//
// Each referenced Secret is fetched at most once per call via the secrets
// cache, no matter how many services reference it. Errors:
//   - ErrSecretNotFound (wrapped): the Secret does not exist — terminal.
//   - errSecretKeyNotFound (wrapped): the key is missing — terminal.
//   - anything else: transient fetch failure — caller should retry next poll.
//
// When the project references no secrets, p is returned unchanged (no copy).
func resolveSecrets(ctx context.Context, client *Client, p *v1.Project) (*v1.Project, error) {
	hasRefs := false
	for _, svc := range p.Spec.Services {
		for _, env := range svc.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				hasRefs = true
			}
		}
	}
	if !hasRefs {
		return p, nil
	}

	// Deep-copy via JSON round-trip so nested slices are not shared with the
	// caller's object.
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets: marshal project: %w", err)
	}
	resolved := &v1.Project{}
	if err := json.Unmarshal(raw, resolved); err != nil {
		return nil, fmt.Errorf("resolve secrets: unmarshal project copy: %w", err)
	}

	// Per-reconcile cache: one GetSecret per referenced Secret name.
	secrets := make(map[string]*v1.Secret)

	for si := range resolved.Spec.Services {
		svc := &resolved.Spec.Services[si]
		for ei := range svc.Env {
			env := &svc.Env[ei]
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			ref := env.ValueFrom.SecretKeyRef

			secret, ok := secrets[ref.Name]
			if !ok {
				secret, err = client.GetSecret(ctx, ref.Name)
				if err != nil {
					return nil, fmt.Errorf("service %q env %q: %w", svc.Name, env.Name, err)
				}
				secrets[ref.Name] = secret
			}

			value, found := "", false
			for _, item := range secret.Spec.Data {
				if item.Key == ref.Key {
					value, found = item.Value, true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("service %q env %q: secret %q: %w: %q",
					svc.Name, env.Name, ref.Name, errSecretKeyNotFound, ref.Key)
			}

			env.Value = value
			env.ValueFrom = nil
		}
	}

	return resolved, nil
}

// terminateOne tears down all Docker resources for a Terminating project and
// reports Terminated back to the server.  The ProjectTerminationController on
// the server will then perform the final store deletion.
//
// Proxy routes are removed after successful teardown.
func terminateOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, p *v1.Project, logger *zap.Logger) {
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
