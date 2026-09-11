package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"go.uber.org/zap"
)

// errSecretKeyNotFound is returned by resolveSecrets when a referenced Secret
// exists but does not contain the requested key. Like ErrSecretNotFound this
// is terminal for the project (Failed, no retry at this layer).
var errSecretKeyNotFound = errors.New("secret key not found")

// secretRefError locates a Secret reference resolveSecrets could not resolve.
// Err is what went wrong — ErrSecretNotFound, errSecretKeyNotFound, or a fetch
// failure — and errors.Is sees through to it.
//
// The fields exist so a caller can describe the failure by name without
// parsing the message. Error() is kept byte-for-byte what resolveSecrets
// returned before this type existed, because reconcileOne publishes it as the
// Project's status message.
type secretRefError struct {
	Service string
	Env     string
	Secret  string
	Key     string
	Err     error
}

func (e *secretRefError) Error() string {
	if errors.Is(e.Err, errSecretKeyNotFound) {
		return fmt.Sprintf("service %q env %q: secret %q: %v: %q", e.Service, e.Env, e.Secret, e.Err, e.Key)
	}
	return fmt.Sprintf("service %q env %q: %v", e.Service, e.Env, e.Err)
}

func (e *secretRefError) Unwrap() error { return e.Err }

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

// coordinatorOf returns the agent-local Coordinator, or nil when backups are
// disabled. It returns a nil interface rather than an interface holding a nil
// pointer, so callers can compare against nil.
func coordinatorOf(backups *BackupSupport) busyChecker {
	if backups == nil || backups.Coordinator == nil {
		return nil
	}
	return backups.Coordinator
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

	// UIDEnforcement turns on Project UID ownership fencing in the orphan sweep
	// (CARA-82). It must match the value passed to docker.NewDockerRuntime so
	// the assignment identities and local container identities are built the
	// same way.
	UIDEnforcement bool

	// PollInterval is how often assigned Projects are fetched and reconciled.
	// Zero means defaultPollInterval. It is configurable so tests can drive the
	// real loop at millisecond cadence; production leaves it unset.
	PollInterval time.Duration

	Logger *zap.Logger
}

// defaultPollInterval is the reconcile cadence. Tier-1 recovery backoffs are
// minimum delays evaluated on this cadence, so a 5s backoff takes effect on
// the first poll after it expires — in practice, 10s.
const defaultPollInterval = 10 * time.Second

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
	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}

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

	// Tier-1 recovery state spans polls: an attempt made on one tick is
	// verified on a later one, and the backoff between attempts is measured
	// against wall-clock time rather than tick count.
	recovery := newRecoveryTracker()

	// ── Bootstrap: health-check Running projects ──────────────────────────
	// After a restart, the Agent has no memory of Running projects. Fetch
	// them from the server and verify containers are still alive so that
	// failures are detected immediately rather than waiting for the first
	// poll tick.
	bootstrapRunningProjects(ctx, client, runtime, routes, coordinatorOf(cfg.Backups), recovery, logger)

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
				State:   v1.NodeStateReady,
				Network: heartbeatNetworkStatus(client, cfg.AgentPort, cfg.AdvertiseIP),
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
			reconcileProjects(ctx, client, runtime, routes, cfg.Backups, orphans, recovery, cfg.UIDEnforcement, logger)
		}
	}
}

func heartbeatNetworkStatus(client *Client, agentPort int, advertiseIP string) v1.NodeNetworkStatus {
	overlayIP := client.OverlayIP()
	if overlayIP == "" {
		overlayIP = advertiseIP
	}
	return v1.NodeNetworkStatus{
		OverlayIP: overlayIP,
		AgentPort: agentPort,
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
func bootstrapRunningProjects(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, busy busyChecker, recovery *recoveryTracker, logger *zap.Logger) {
	projects, err := client.ListProjectsAssignedToNode(ctx)
	if err != nil {
		logger.Warn("Bootstrap: failed to list projects", zap.Error(err))
		return
	}

	var running int
	for _, p := range projects {
		if p.Status.Phase == v1.ProjectPhaseRunning {
			running++
			healthCheckOne(ctx, client, runtime, routes, busy, recovery, p, logger)
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
func reconcileProjects(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, backups *BackupSupport, orphans *orphanTracker, recovery *recoveryTracker, uidEnforcement bool, logger *zap.Logger) {
	assignedProjects, err := client.ListProjectsAssignedToNode(ctx)
	if err != nil {
		// Unknown ownership never counts toward destructive cleanup. Preserve
		// already-stopped state, but require a fresh full grace period after the
		// server becomes reachable again.
		if orphans != nil {
			orphans.resetGrace()
		}
		logger.Warn("Failed to list projects for reconcile", zap.Error(err))
		return
	}

	busy := coordinatorOf(backups)

	// The orphan sweep must see the complete node ownership snapshot, including
	// Failed projects. Filtering phases before this point would misclassify a
	// Failed-but-still-owned project as an orphan.
	if orphans != nil {
		sweepOrphans(ctx, runtime, routes, orphans, busy, assignedProjects, uidEnforcement, logger)
	}

	projects := projectsForReconcile(assignedProjects)

	// Sync before the early return: an empty list means every Project left
	// this node, and their backup goroutines must be cancelled.
	if backups != nil && backups.Supervisor != nil {
		backups.Supervisor.Sync(ctx, projects)
	}

	// Same reasoning for recovery state, and the same reason it is safe here:
	// assignedProjects is the complete ownership snapshot, so an entry missing
	// from it belongs to a Project this node no longer runs — moved away,
	// deleted, or reassigned under a new generation. Reaching this line at all
	// means the server answered, so an empty set means "nothing is ours", not
	// "we could not tell".
	if recovery != nil {
		owned := make(map[recoveryKey]struct{}, len(assignedProjects))
		for _, p := range assignedProjects {
			owned[keyForProject(p)] = struct{}{}
		}
		recovery.retain(owned)
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
			healthCheckOne(ctx, client, runtime, routes, busy, recovery, p, logger)
		default:
			reconcileOne(ctx, client, runtime, routes, backups, p, logger)
		}
	}
}

func projectsForReconcile(projects []*v1.Project) []*v1.Project {
	filtered := make([]*v1.Project, 0, len(projects))
	for _, project := range projects {
		switch project.Status.Phase {
		case v1.ProjectPhaseScheduled, v1.ProjectPhaseRunning, v1.ProjectPhaseTerminating:
			filtered = append(filtered, project)
		}
	}
	return filtered
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
		_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p),
			v1.ProjectPhaseFailed,
			"InspectError",
			err.Error(),
		)
		return
	}

	// A container under a service's name that belongs to another assignment
	// is checked before anything is counted. Counted as it is found, a stale
	// container that is running would satisfy this assignment — the Project
	// reported Running and proxy routes pointed at a workload from an earlier
	// grant — and one that exited with an error would fail it. Nor may the
	// reconcile below run: ensureContainer would refuse to adopt the stale
	// container, and the failure path must not be what removes it. So the
	// Project is reported blocked, its phase left as it is, and Docker left
	// alone, exactly as healthCheckOne does for a Running Project.
	for _, s := range states {
		if s.NotOwned != nil {
			log.Warn("A container under a service's name belongs to another assignment",
				zap.String("reason", recoveryBlockedStaleContainer),
				zap.String("service", s.ServiceName), zap.Error(s.NotOwned))
			reportRecoveryBlocked(ctx, client, p, recoveryBlockedStaleContainer, staleContainerMessage(s.NotOwned), log)
			return
		}
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
		_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, "ContainerExited", msg)
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
		if err := client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p),
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
			_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, "SecretNotFound", err.Error())
		case errors.Is(err, errSecretKeyNotFound):
			log.Warn("Referenced secret key does not exist", zap.Error(err))
			_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, "SecretKeyNotFound", err.Error())
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
			_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, "RestoreError", err.Error())
			return
		}
	}

	if err := runtime.ReconcileProject(ctx, resolved); err != nil {
		log.Error("Failed to reconcile project", zap.Error(err))
		_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, "ReconcileError", err.Error())
		return
	}

	if err := client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p),
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
	return resolveSecretsFor(ctx, client, p, func(string) bool { return true })
}

// resolveSecretsFor is resolveSecrets restricted to the services include
// accepts. References in other services are left unresolved in the copy.
//
// Recovery needs the restriction. It only rebuilds the services that broke,
// and a Secret referenced only by a service that is still running is not a
// precondition for that — the running container already holds its
// environment. Resolving every service would let a deleted Secret belonging
// to a healthy service block the recovery of an unrelated one.
func resolveSecretsFor(ctx context.Context, client *Client, p *v1.Project, include func(service string) bool) (*v1.Project, error) {
	hasRefs := false
	for _, svc := range p.Spec.Services {
		if !include(svc.Name) {
			continue
		}
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
		if !include(svc.Name) {
			continue
		}
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
					return nil, &secretRefError{Service: svc.Name, Env: env.Name, Secret: ref.Name, Key: ref.Key, Err: err}
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
				return nil, &secretRefError{Service: svc.Name, Env: env.Name, Secret: ref.Name, Key: ref.Key,
					Err: errSecretKeyNotFound}
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
		_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p),
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
	if err := client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p),
		v1.ProjectPhaseTerminated,
		"ResourcesRemoved",
		"All Docker resources have been removed",
	); err != nil {
		log.Warn("Failed to update project status to Terminated", zap.Error(err))
	}
}

// healthCheckOne inspects a Running project's containers and decides what to
// do about what it finds: nothing if every container is running; hold off if
// a container is restarting or the Project is under maintenance; report
// Failed for a fault tier-1 recovery must not touch — a paused or
// unrecognised container, a restart stuck past its window, or an inspect that
// failed; and otherwise hand the broken services to recoverLocally, which
// restarts them on this node within a bounded number of attempts. With a nil
// recovery tracker it only reports, as before CARA-86.
//
// Moving a Project to another node is not done here; that belongs to the
// server-side rescheduler.
//
// A container that does not belong to the current assignment is never judged
// at all: it is reported as RecoveryBlocked/StaleContainer and left alone.
//
// For healthy projects, proxy routes are re-affirmed to handle container IP
// changes after a restart.
func healthCheckOne(ctx context.Context, client *Client, runtime docker.Runtime, routes RouteUpdater, busy busyChecker, recovery *recoveryTracker, p *v1.Project, logger *zap.Logger) {
	log := logger.With(zap.String("project", p.Name))
	key := keyForProject(p)

	// clearTransient forgets any restart timer for this assignment. It is
	// called on every path that ends the deferral — healthy, maintenance, and
	// each report of Failed — so a timer never outlives the judgement it was
	// timing. retain alone cannot guarantee that: a Failed Project still
	// assigned here stays in the ownership snapshot.
	clearTransient := func() {
		if recovery != nil {
			recovery.clearTransient(key)
		}
	}

	// reportFailed is the single way this function reports Failed, so none of
	// those paths can leave a restart timer behind.
	reportFailed := func(reason, message string) {
		clearTransient()
		_ = client.UpdateProjectStatus(ctx, p.Name, fenceForProject(p), v1.ProjectPhaseFailed, reason, message)
	}

	// A backup stops a Project's containers on purpose. reconcileProjects
	// already skips a Project whose coordinator claim is held, which covers
	// operations this agent is running right now; Maintenance covers the case
	// that check cannot see — the agent restarted mid-backup, so the in-memory
	// claim is gone while the Project is still deliberately stopped.
	//
	// It is checked before anything else, the Docker inspect included: under
	// maintenance nothing about the containers is judged, so an inspect that
	// happened to fail must not be reported either. The restart timer is reset
	// too — a container that was restarting when the backup began has not
	// been stuck for the length of the backup.
	//
	// This reads the Project from the poll, which may be seconds old. That is
	// enough to hold off judgement; recoverLocally checks again on a fresh copy
	// before it acts, which is what closes the race with a backup that began
	// since.
	if v1.IsMaintenanceActive(p.Status.Conditions, time.Now()) {
		clearTransient()
		log.Info("Project is under maintenance, not health-checking")
		return
	}

	states, err := runtime.InspectProject(ctx, p)
	if err != nil {
		log.Warn("Failed to inspect project containers", zap.Error(err))
		reportFailed("InspectError", err.Error())
		return
	}

	bad := unhealthy(classifyProject(p, states))

	// A container under a service's name that belongs to another assignment
	// comes before everything else, the healthy check included. Found by name
	// alone, a stale container that is running would read as a healthy
	// Project: the RecoveryBlocked condition would be cleared, proxy routes
	// pointed at the stale container, and recovery never considered. Nothing
	// here is this assignment's to judge or to act on, so it is reported as
	// blocked and left exactly as it is — no route update, no condition
	// cleared, no Docker call. Only the orphan sweep or an operator can
	// resolve it.
	if stale := firstNotOwned(bad); stale != nil {
		clearTransient()
		log.Warn("A container under a service's name belongs to another assignment",
			zap.String("reason", recoveryBlockedStaleContainer),
			zap.String("detail", detailList(bad)), zap.Error(stale.NotOwned))
		reportRecoveryBlocked(ctx, client, p, recoveryBlockedStaleContainer, staleContainerMessage(stale.NotOwned), log)
		return
	}

	if len(bad) == 0 {
		// All containers are running — healthy.
		// Re-affirm proxy routes to handle container IP changes after restart.
		log.Debug("All containers healthy, nothing to do")
		if recovery != nil {
			recovery.observeHealthy(key)
		}
		clearTransient()
		// A block can end without recovery ever running again: an operator
		// restores the data and starts the container by hand. The condition
		// would then claim cara refuses to act on a Project that is fine.
		clearRecoveryBlocked(ctx, client, p, log)
		updateProxyRoutes(ctx, runtime, routes, p, log)
		return
	}

	reason, summary := failureReason(bad)
	msg := summary + ": " + detailList(bad)

	// What is found decides what happens, in this order.
	//
	// 1. Paused or unrecognised — Failed, and nothing is touched. Nothing in
	//    cara pauses a container, so a paused one means something outside cara
	//    acted on a container cara owns, and resuming it would fight whoever
	//    did that. For a status this code does not recognise there is no
	//    operation that can be called narrow or safe. Either needs a human,
	//    and that stays true whatever the other services are doing — so this
	//    wins even over a service that is restarting.
	if needsHuman(bad) {
		log.Warn("Project is unhealthy", zap.String("reason", reason), zap.String("detail", msg))
		reportFailed(reason, msg)
		return
	}

	// 2. Anything restarting — judgement on the whole Project is deferred.
	//    Docker is changing its state right now, and the next poll will see
	//    where it settles. Recovering the other services in the meantime would
	//    mean acting on a picture that is already out of date, and starting a
	//    second round of changes while the first is still in flight: a web
	//    service started while its database is mid-restart is the case this
	//    avoids. No attempt is spent — nothing has been tried.
	//
	//    Deferral is bounded. Reporting Failed on the first sighting would be
	//    worse than waiting, because Failed is terminal for the poll loop and a
	//    container about to come back would be stranded; but one that never
	//    comes back must not leave the Project reading Running for good.
	if anyTransient(bad) {
		if recovery != nil && recovery.observeTransient(key) {
			stuck := fmt.Sprintf("Containers stuck restarting for over %s: %s",
				transientObservationTimeout, detailList(bad))
			log.Warn("Containers are stuck restarting", zap.String("detail", stuck))
			reportFailed("ContainerRestartStuck", stuck)
			return
		}
		log.Info("Containers are restarting, deferring judgement",
			zap.String("detail", detailList(bad)))
		return
	}
	clearTransient()

	// 3. Exited, created, missing or dead — tier-1 recovery, when enabled.
	if recovery == nil || !recoverable(bad) {
		log.Warn("Project is unhealthy", zap.String("reason", reason), zap.String("detail", msg))
		reportFailed(reason, msg)
		return
	}

	recoverLocally(ctx, client, runtime, busy, recovery, p, bad, log)
}

// recoverLocally runs one round of tier-1 recovery for an unhealthy Project.
//
// The order below is the design. Every check that can refuse the round runs
// before recovery.next(), because next() is what spends an attempt: a round
// refused for a reason that is not a failed restart — another operation holds
// the Project, the server did not answer, the Project moved, a backup is under
// way, its data is gone — must not bring the Project closer to
// LocalRestartExhausted. Only a round that is about to call RecoverServices
// counts.
//
//	claim OpRecovery          another operation holds it → skip
//	re-read the Project       server unreachable         → skip
//	fence: UID, generation,   anything differs           → skip
//	  nodeRef, Running
//	fresh Maintenance         active                     → skip
//	PreflightRecovery         stale container            → RecoveryBlocked
//	                          volume data absent         → RecoveryBlocked
//	resolve Secrets           Secret or key absent       → RecoveryBlocked
//	                          fetch failed               → skip
//	clear RecoveryBlocked     every precondition holds
//	recovery.next()           the attempt is spent here
//	RecoverServices           repeats the preflight itself
//	release the claim         deferred
//
// The claim is held across all of it, both preflights included, so a backup
// cannot stop the containers between the decision to recover and the Docker
// calls that act on it.
//
// The Project phase is deliberately left alone. A Project whose containers the
// agent is putting back is still Running as far as the control plane is
// concerned; flipping it to Failed and back would make every recovery look
// like an outage and would hand the Project to paths that are meant to be
// closed while it runs.
func recoverLocally(
	ctx context.Context,
	client *Client,
	runtime docker.Runtime,
	busy busyChecker,
	recovery *recoveryTracker,
	p *v1.Project,
	bad []serviceState,
	log *zap.Logger,
) {
	key := keyForProject(p)
	names := serviceNames(bad)
	detail := detailList(bad)

	// ── Claim ─────────────────────────────────────────────────────────────
	//
	// reconcileProjects already skipped this Project if it was busy, but that
	// was a check, not a claim: a backup tick can start between it and here.
	// Claiming closes that gap and keeps it closed until the recovery ends.
	if busy != nil {
		release, ok := busy.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpRecovery)
		if !ok {
			log.Info("Another operation holds the Project, not recovering this tick",
				zap.String("detail", detail))
			return
		}
		defer release()
	}

	// ── Re-read and fence ─────────────────────────────────────────────────
	//
	// The Project in hand came from a poll that may be seconds old, and in
	// that window it could have been reassigned, moved to Terminating, or
	// had a backup mark it Maintenance. Acting on the old copy would restart
	// containers under a grant that has moved — exactly the double-run the
	// assignment fence exists to prevent — and the fence on the status write
	// afterwards would be too late, because Docker would already have been
	// mutated.
	current, err := client.GetProject(ctx, p.Name)
	if err != nil {
		log.Warn("Could not re-read the Project before recovery, skipping this tick",
			zap.Error(err))
		return
	}
	if err := checkRecoveryFence(p, current, client.NodeName()); err != nil {
		log.Info("Project changed before recovery could run, skipping", zap.Error(err))
		return
	}

	// The copy healthCheckOne checked was the stale one. A backup that began
	// since then is visible only here.
	if v1.IsMaintenanceActive(current.Status.Conditions, time.Now()) {
		log.Info("Project entered maintenance before recovery could run, skipping",
			zap.String("detail", detail))
		return
	}

	// ── Volume preflight: the attempt-accounting gate ─────────────────────
	//
	// This decides whether an attempt is worth spending, not whether the
	// recovery is safe. RecoverServices repeats the same checks immediately
	// before touching Docker, and that second check is the one data safety
	// rests on.
	if err := runtime.PreflightRecovery(ctx, current, names); err != nil {
		if errors.Is(err, docker.ErrRecoveryVolumeUnavailable) {
			// Missing data does not come back by waiting, so this is not a
			// failed attempt and not a step towards exhaustion. It is left
			// for a restore or an operator, and re-checked every tick so
			// recovery resumes on its own once the data is back. The phase
			// stays Running for the same reason: Failed would take the
			// Project out of the poll loop for good.
			log.Warn("Recovery blocked: volume data the services need is not on this node",
				zap.String("reason", recoveryBlockedVolumeUnavailable),
				zap.Strings("services", names), zap.Error(err))
			reportRecoveryBlocked(ctx, client, current, recoveryBlockedVolumeUnavailable, volumeBlockedMessage(err), log)
			return
		}
		if errors.Is(err, docker.ErrContainerNotOwned) {
			// A container under the service's name belongs to another
			// assignment. Starting it would run a stale grant's workload as
			// this one, and removing it would destroy something this
			// assignment does not own. Like missing data, waiting does not
			// change that — the orphan sweep or an operator has to — so it
			// blocks rather than spending attempts.
			log.Warn("Recovery blocked: a container under the service's name belongs to another assignment",
				zap.String("reason", recoveryBlockedStaleContainer),
				zap.Strings("services", names), zap.Error(err))
			reportRecoveryBlocked(ctx, client, current, recoveryBlockedStaleContainer, staleContainerMessage(err), log)
			return
		}
		log.Warn("Recovery preflight failed, skipping this tick",
			zap.Strings("services", names), zap.Error(err))
		return
	}

	// ── Secrets, from the Project just read ───────────────────────────────
	//
	// A missing or dead container is rebuilt from scratch, so its environment
	// has to be resolved exactly as reconcileOne does for a first start. The
	// fresh copy is used so a Secret rotated since the poll is picked up, and
	// only the services being recovered are resolved.
	//
	// A Secret or key that does not exist is the same kind of fault as a
	// missing volume: not a failed attempt but a precondition that does not
	// hold, and will not start holding by waiting. It blocks the round the
	// same way. A fetch that failed is different — the server was busy or
	// unreachable — and is simply retried next poll.
	resolved, err := resolveSecretsFor(ctx, client, current, func(service string) bool {
		return slices.Contains(names, service)
	})
	if err != nil {
		if reason, message, blocked := secretBlocked(err); blocked {
			log.Warn("Recovery blocked: a Secret the services need does not exist",
				zap.String("reason", reason), zap.Strings("services", names), zap.Error(err))
			reportRecoveryBlocked(ctx, client, current, reason, message, log)
			return
		}
		log.Warn("Could not fetch Secrets for recovery, will retry next poll",
			zap.Strings("services", names), zap.Error(err))
		return
	}

	// Every precondition holds, so whatever blocked an earlier round no longer
	// does. Cleared here — after the volumes and the Secrets, before the
	// attempt — rather than after the recovery succeeds: the condition says
	// "cara will not act", and from here on it will. Clearing any earlier
	// would briefly publish "not blocked" for a Project a later check was
	// about to block again.
	clearRecoveryBlocked(ctx, client, current, log)

	// ── Attempt accounting ────────────────────────────────────────────────
	switch recovery.next(key) {
	case recoveryWait:
		log.Info("Waiting before the next recovery attempt",
			zap.Int("attempts", recovery.attempts(key)), zap.String("detail", detail))
		return

	case recoveryExhausted:
		exhausted := fmt.Sprintf("Local recovery exhausted after %d attempts: %s",
			maxRecoveryAttempts, detail)
		log.Warn("Local recovery exhausted", zap.String("detail", exhausted))
		_ = client.UpdateProjectStatus(ctx, current.Name, fenceForProject(current),
			v1.ProjectPhaseFailed, "LocalRestartExhausted", exhausted)
		return
	}

	attempt := recovery.attempts(key)
	log.Info("Recovering containers locally",
		zap.Int("attempt", attempt), zap.Strings("services", names), zap.String("detail", detail))

	// ── Docker ────────────────────────────────────────────────────────────
	//
	// Only the services that are actually broken are touched, and only by
	// RecoverServices: neither ReconcileProject nor StartProject can be used
	// on a Project that is already serving traffic. ReconcileProject rolls
	// back by removing the whole Project when any step fails, which would turn
	// one container that could not be recreated into the loss of every healthy
	// container and its Ephemeral volumes. StartProject is safe but too
	// narrow — it cannot recreate a container that is missing or dead.
	if err := runtime.RecoverServices(ctx, resolved, names); err != nil {
		if errors.Is(err, docker.ErrContainerNotOwned) {
			// A stale container appeared between the gate above and here.
			// RecoverServices refused before touching it; the next round's
			// gate will report the block.
			log.Warn("Recovery aborted: a container became another assignment's after the preflight",
				zap.String("reason", recoveryBlockedStaleContainer),
				zap.Int("attempt", attempt), zap.Error(err))
			return
		}
		if errors.Is(err, docker.ErrRecoveryVolumeUnavailable) {
			// The data vanished between the gate above and here. Nothing was
			// mutated — RecoverServices refuses before its first Docker call —
			// but the attempt is spent. That is deliberate: recovery had
			// formally begun, and handing the attempt back would make the
			// tracker's state depend on where inside an attempt it failed.
			log.Warn("Recovery aborted: volume data disappeared after the preflight",
				zap.String("reason", "RecoveryBlocked"),
				zap.Int("attempt", attempt), zap.Error(err))
			return
		}
		// Reporting nothing here is deliberate: the next poll re-observes the
		// containers and decides, so a failure that fixed itself does not
		// leave a stale error behind.
		log.Warn("Recovery attempt failed",
			zap.Strings("services", names), zap.Int("attempt", attempt), zap.Error(err))
		return
	}

	log.Info("Recovery attempt completed, awaiting verification",
		zap.Strings("services", names), zap.Int("attempt", attempt))
}

// checkRecoveryFence reports why current is no longer the assignment that was
// observed unhealthy, or nil if it still is.
//
// All four conditions are needed. UID and generation identify the grant: a
// delete-and-recreate changes the first, a reassignment away and back changes
// the second while leaving nodeRef pointing here again. nodeRef catches a
// reassignment the generation alone would also catch, but checking it against
// this node's own name is what makes "ours" explicit rather than inferred.
// Running catches Terminating, where recovering would resurrect containers the
// control plane is tearing down.
func checkRecoveryFence(observed, current *v1.Project, nodeName string) error {
	switch {
	case current.Namespace != observed.Namespace || current.Name != observed.Name:
		return fmt.Errorf("project identity changed from %s/%s to %s/%s",
			observed.Namespace, observed.Name, current.Namespace, current.Name)
	case current.UID != observed.UID:
		return fmt.Errorf("project UID changed from %q to %q", observed.UID, current.UID)
	case current.Status.AssignmentGeneration != observed.Status.AssignmentGeneration:
		return fmt.Errorf("assignment generation changed from %d to %d",
			observed.Status.AssignmentGeneration, current.Status.AssignmentGeneration)
	case current.Status.NodeRef != nodeName:
		return fmt.Errorf("project is assigned to node %q, not this node %q",
			current.Status.NodeRef, nodeName)
	case current.Status.Phase != v1.ProjectPhaseRunning:
		return fmt.Errorf("project phase is %s, not Running", current.Status.Phase)
	}
	return nil
}

// RecoveryBlocked reasons. Each names a precondition that does not hold and
// that waiting will not fix.
const (
	recoveryBlockedVolumeUnavailable = "VolumeUnavailable"
	recoveryBlockedSecretNotFound    = "SecretNotFound"
	recoveryBlockedSecretKeyNotFound = "SecretKeyNotFound"
	recoveryBlockedStaleContainer    = "StaleContainer"
)

// The condition messages below name only what the Project's own spec names —
// services, volumes, Secrets, keys. The full error, with host paths and
// operating-system detail, goes to the agent log; Project status is readable
// by anyone who can read the Project, and the node's filesystem layout is not
// theirs to see. The same names also keep the message stable from one poll to
// the next, which is what lets reportRecoveryBlocked skip repeats.

// volumeBlockedMessage describes a missing volume by name.
func volumeBlockedMessage(err error) string {
	var vu *docker.VolumeUnavailableError
	if errors.As(err, &vu) {
		return fmt.Sprintf("service %q needs %s volume %q, which is not available on this node",
			vu.Service, vu.Type, vu.Volume)
	}
	return "a volume the services need is not available on this node"
}

// staleContainerMessage describes a container that is not this assignment's,
// by service name. The labels that did not match stay in the agent log.
func staleContainerMessage(err error) string {
	var no *docker.ContainerNotOwnedError
	if errors.As(err, &no) {
		return fmt.Sprintf("service %q has a container left by another assignment, which cara will neither start nor remove",
			no.Service)
	}
	return "a service has a container left by another assignment, which cara will neither start nor remove"
}

// secretBlocked classifies a resolveSecretsFor error. It reports blocked only
// for a Secret or key that does not exist; a fetch failure is not a block.
func secretBlocked(err error) (reason, message string, blocked bool) {
	var ref *secretRefError
	if !errors.As(err, &ref) {
		return "", "", false
	}
	switch {
	case errors.Is(err, ErrSecretNotFound):
		return recoveryBlockedSecretNotFound,
			fmt.Sprintf("service %q needs Secret %q, which does not exist", ref.Service, ref.Secret), true
	case errors.Is(err, errSecretKeyNotFound):
		return recoveryBlockedSecretKeyNotFound,
			fmt.Sprintf("service %q needs key %q in Secret %q, which does not exist",
				ref.Service, ref.Key, ref.Secret), true
	default:
		return "", "", false
	}
}

// reportRecoveryBlocked sets the RecoveryBlocked condition on p, unless p
// already carries exactly this one.
//
// The comparison is what keeps this cheap. A blocked Project is re-evaluated
// on every poll, and the server stamps a fresh timestamp on each condition
// write, so writing unconditionally would mean a database write and a
// project.updated event every ten seconds for as long as the data stays
// missing — each one announcing nothing. p is the copy recoverLocally has just
// re-read, so what it says is at most one round trip old.
//
// A block with a different reason — the volume is back but a Secret is now
// missing — is written as one PATCH that replaces the old condition, never a
// DELETE and then a PATCH, so there is no moment at which the Project reads as
// unblocked.
//
// A failed write is logged and not retried here: the condition is how the
// control plane learns of the block, not what enforces it, and the next poll
// will try again because p will still not carry it.
func reportRecoveryBlocked(ctx context.Context, client *Client, p *v1.Project, reason, message string, log *zap.Logger) {
	if c, ok := projectCondition(p, v1.ConditionTypeRecoveryBlocked); ok &&
		c.Status == v1.ConditionTrue && c.Reason == reason && c.Message == message {
		return
	}
	if err := client.PatchProjectCondition(ctx, p.Name, fenceForProject(p),
		v1.ConditionTypeRecoveryBlocked, v1.ConditionTrue, reason, message); err != nil {
		log.Warn("Failed to report RecoveryBlocked condition", zap.Error(err))
	}
}

// clearRecoveryBlocked removes the RecoveryBlocked condition from p if p
// carries one. Like reportRecoveryBlocked it writes only on a change, and a
// failure is left for the next poll.
func clearRecoveryBlocked(ctx context.Context, client *Client, p *v1.Project, log *zap.Logger) {
	if _, ok := projectCondition(p, v1.ConditionTypeRecoveryBlocked); !ok {
		return
	}
	if err := client.ClearProjectCondition(ctx, p.Name, fenceForProject(p),
		v1.ConditionTypeRecoveryBlocked); err != nil {
		log.Warn("Failed to clear RecoveryBlocked condition", zap.Error(err))
	}
}

func projectCondition(p *v1.Project, condType v1.ConditionType) (v1.Condition, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == condType {
			return c, true
		}
	}
	return v1.Condition{}, false
}

// serviceNames lists the services a recovery attempt is allowed to touch.
func serviceNames(bad []serviceState) []string {
	names := make([]string, 0, len(bad))
	for _, s := range bad {
		names = append(names, s.Service)
	}
	return names
}

// recoverable reports whether every fault present is one tier-1 recovery knows
// a safe operation for.
func recoverable(bad []serviceState) bool {
	for _, s := range bad {
		switch s.Health {
		case healthStartable, healthNeedsRecreate:
		default:
			return false
		}
	}
	return len(bad) > 0
}

// needsHuman reports whether any fault is one tier-1 recovery must not act on
// at all: a paused container, or a status this code does not recognise.
func needsHuman(bad []serviceState) bool {
	for _, s := range bad {
		switch s.Health {
		case healthPaused, healthUnknown:
			return true
		}
	}
	return false
}

// firstNotOwned returns the first service whose container belongs to another
// assignment, or nil.
func firstNotOwned(bad []serviceState) *serviceState {
	for i := range bad {
		if bad[i].Health == healthNotOwned {
			return &bad[i]
		}
	}
	return nil
}

// anyTransient reports whether any unhealthy service is mid-action in Docker.
func anyTransient(bad []serviceState) bool {
	for _, s := range bad {
		if s.Health == healthTransient {
			return true
		}
	}
	return false
}

// detailList renders every unhealthy service, so a Project with two different
// faults reports both rather than only whichever the first check happened to
// match.
func detailList(bad []serviceState) string {
	parts := make([]string, 0, len(bad))
	for _, s := range bad {
		parts = append(parts, s.Detail())
	}
	return strings.Join(parts, ", ")
}

// failureReason picks the condition reason from the most severe fault present.
//
// The first three reasons are the ones this code already reported and are kept
// byte-for-byte: anything watching for them keeps working. ContainerUnhealthy
// is new, and covers the states that previously fell through every check and
// were counted as healthy — dead, paused, and any status this code does not
// recognise.
func failureReason(bad []serviceState) (reason, summary string) {
	var missing, dead, crashed, exited, paused, restarting, unknown bool
	for _, s := range bad {
		switch {
		case s.Crashed():
			crashed = true
		case s.Health == healthNeedsRecreate && s.Status == "":
			missing = true
		case s.Health == healthNeedsRecreate:
			dead = true
		case s.Health == healthStartable:
			exited = true
		case s.Health == healthPaused:
			paused = true
		case s.Health == healthTransient:
			restarting = true
		default:
			unknown = true
		}
	}

	switch {
	case crashed:
		return "ContainerCrashed", "Containers crashed"
	case missing:
		return "ContainerMissing", "Missing containers for services"
	case exited:
		return "ContainerExited", "Containers exited cleanly"
	case dead:
		return "ContainerUnhealthy", "Containers are dead"
	case paused:
		return "ContainerUnhealthy", "Containers are paused"
	case unknown:
		return "ContainerUnhealthy", "Containers are in an unrecognised state"
	case restarting:
		return "ContainerUnhealthy", "Containers are restarting"
	default:
		return "ContainerUnhealthy", "Containers are not running"
	}
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
