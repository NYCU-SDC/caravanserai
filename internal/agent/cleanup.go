package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"go.uber.org/zap"
)

// CleanupResource is one kind of runtime resource a Node can hold for an
// assignment. ResourcesRemoved is reported only when every registered kind
// has nothing left for the assignment, so a feature that gives a Node a new
// deletion-scoped resource — a repository authorization, a client certificate
// file — registers a kind here and is covered by the same evidence without
// changing it.
//
// Managed volume data is not a kind: it is retained on deletion, and when a
// Project moves it is quarantined with its own evidence (see runCleanupTask).
type CleanupResource interface {
	// Kind names the resource in logs.
	Kind() string
	// Remove removes everything of this kind held for a. It is idempotent:
	// what is already gone is not an error.
	Remove(ctx context.Context, a docker.Assignment) error
	// Remaining describes what of this kind is still held for a, read fresh.
	// Empty means none.
	Remaining(ctx context.Context, a docker.Assignment) ([]string, error)
}

// assignmentCleaner removes and accounts for every registered resource kind.
type assignmentCleaner struct {
	resources []CleanupResource

	// dataRoot is where this Node's Managed data lives. Without one, local data
	// stays owed rather than being reported as handled.
	dataRoot string
}

func newAssignmentCleaner(resources ...CleanupResource) *assignmentCleaner {
	return &assignmentCleaner{resources: resources}
}

// removeAll removes every kind held for a, attempting each even when an
// earlier one fails, so one stuck kind does not hold the others back.
func (c *assignmentCleaner) removeAll(ctx context.Context, a docker.Assignment) error {
	var errs []error
	for _, r := range c.resources {
		if err := r.Remove(ctx, a); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.Kind(), err))
		}
	}
	return errors.Join(errs...)
}

// remaining lists what is still held for a across every kind. Any kind that
// cannot be read makes the answer unknown, which is an error, never "none".
func (c *assignmentCleaner) remaining(ctx context.Context, a docker.Assignment) ([]string, error) {
	var left []string
	for _, r := range c.resources {
		items, err := r.Remaining(ctx, a)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Kind(), err)
		}
		left = append(left, items...)
	}
	return left, nil
}

// assignmentRuntime is the part of the Docker runtime the cleanup uses. It is
// not on docker.Runtime so the many existing runtime fakes need not grow it.
type assignmentRuntime interface {
	RemoveAssignment(ctx context.Context, a docker.Assignment) error
	AssignmentRemaining(ctx context.Context, a docker.Assignment) ([]string, error)
}

// dockerResources is the Docker kind: containers of the assignment, and the
// lifetime's network and Ephemeral volumes once no other generation uses them.
type dockerResources struct{ runtime assignmentRuntime }

func (dockerResources) Kind() string { return "docker" }

func (d dockerResources) Remove(ctx context.Context, a docker.Assignment) error {
	return d.runtime.RemoveAssignment(ctx, a)
}

func (d dockerResources) Remaining(ctx context.Context, a docker.Assignment) ([]string, error) {
	return d.runtime.AssignmentRemaining(ctx, a)
}

// proxyRoutes is the ingress kind. Routes are kept in memory by Project name,
// so removing them cannot be checked afterwards; Remaining is always empty.
// Removing by name is safe only because a cleanup task acts after confirming
// that this Node holds no assignment of the Project at all.
type proxyRoutes struct{ routes RouteUpdater }

func (proxyRoutes) Kind() string { return "proxy-routes" }

func (p proxyRoutes) Remove(_ context.Context, a docker.Assignment) error {
	if p.routes != nil {
		p.routes.Remove(a.Project)
	}
	return nil
}

func (proxyRoutes) Remaining(context.Context, docker.Assignment) ([]string, error) { return nil, nil }

// defaultCleaner registers the kinds every Agent has. It returns nil when the
// runtime cannot remove a single assignment, which leaves cleanup tasks
// unhandled rather than handled by a broader removal.
func defaultCleaner(runtime docker.Runtime, routes RouteUpdater, dataRoot string) *assignmentCleaner {
	ar, ok := runtime.(assignmentRuntime)
	if !ok {
		return nil
	}
	cleaner := newAssignmentCleaner(dockerResources{runtime: ar}, proxyRoutes{routes: routes})
	cleaner.dataRoot = dataRoot
	return cleaner
}

func assignmentOf(namespace, project string, id v1.AssignmentIdentity) docker.Assignment {
	if namespace == "" {
		namespace = v1.DefaultNamespace
	}
	return docker.Assignment{Namespace: namespace, Project: project, UID: id.ProjectUID, Generation: id.AssignmentGeneration}
}

// cleanupServer is the part of the server API the cleanup tasks use.
type cleanupServer interface {
	NodeName() string
	ListCleanupTasks(ctx context.Context) ([]v1.CleanupTask, error)
	GetProject(ctx context.Context, name string) (*v1.Project, error)
	ReportResourcesRemoved(ctx context.Context, projectName string, assignment v1.AssignmentIdentity) error
	ReportManagedDataQuarantined(ctx context.Context, projectName string, assignment v1.AssignmentIdentity) error
}

// runCleanupTasks works through the cleanup obligations this Node owes for
// assignments it has given up.
//
// The listing is only a list of candidates. Each task is acted on under the
// Project's Coordinator claim — the same OpOrphanCleanup claim the orphan
// sweep takes, so the two never run on one Project at once — and only after
// the server is asked again, under that claim, whether the obligation still
// stands. A task that cannot be claimed is skipped until the next poll rather
// than queued behind whatever holds the claim.
func runCleanupTasks(ctx context.Context, server cleanupServer, cleaner *assignmentCleaner, busy busyChecker, logger *zap.Logger) {
	if cleaner == nil {
		return
	}
	tasks, err := server.ListCleanupTasks(ctx)
	if err != nil {
		logger.Warn("Failed to list cleanup obligations", zap.Error(err))
		return
	}
	for _, task := range tasks {
		runCleanupTask(ctx, server, cleaner, busy, task, logger)
	}
}

func runCleanupTask(ctx context.Context, server cleanupServer, cleaner *assignmentCleaner, busy busyChecker, task v1.CleanupTask, logger *zap.Logger) {
	a := assignmentOf(task.Namespace, task.ProjectName, task.Assignment)
	log := logger.With(zap.String("project", task.ProjectName),
		zap.String("uid", a.UID), zap.Int64("generation", a.Generation))

	if busy != nil {
		release, ok := busy.TryClaim(backup.ResourceKey{Namespace: a.Namespace, Name: a.Project}, backup.OpOrphanCleanup)
		if !ok {
			log.Debug("Deferring cleanup: project busy with another operation")
			return
		}
		defer release()
	}

	obligation, reason := revalidateCleanup(ctx, server, task)
	if reason != "" {
		log.Info("Skipping cleanup task", zap.String("reason", reason))
		return
	}

	// Removing and confirming run even when ResourcesRemoved is already on
	// record: local data may move only once no container can still be using it.
	if err := cleaner.removeAll(ctx, a); err != nil {
		log.Warn("Cleanup incomplete, will retry next poll", zap.Error(err))
		return
	}
	left, err := cleaner.remaining(ctx, a)
	if err != nil {
		log.Warn("Could not confirm cleanup, will retry next poll", zap.Error(err))
		return
	}
	if len(left) > 0 {
		log.Warn("Resources still present after cleanup, will retry next poll",
			zap.String("remaining", strings.Join(left, ", ")))
		return
	}

	if !obligation.Resolved() && !reportResourcesRemoved(ctx, server, task, log) {
		return
	}
	if obligation.DataOutstanding() {
		quarantineManagedData(ctx, server, cleaner, a, task, log)
	}
}

// quarantineManagedData moves the assignment's local Managed data out of the
// active path and reports it. A failure reports nothing: the obligation stays
// owed, the Node stays excluded from the Project, and the next poll retries.
func quarantineManagedData(ctx context.Context, server cleanupServer, cleaner *assignmentCleaner, a docker.Assignment, task v1.CleanupTask, log *zap.Logger) {
	if cleaner.dataRoot == "" {
		log.Warn("No data root configured; local Managed data stays owed")
		return
	}
	owner := restore.Provenance{
		Namespace: a.Namespace, Project: a.Project, ProjectUID: a.UID,
		NodeName: server.NodeName(), AssignmentGeneration: a.Generation,
	}
	if err := restore.Quarantine(cleaner.dataRoot, owner, nowUTC()); err != nil {
		log.Warn("Could not quarantine local Managed data, will retry next poll", zap.Error(err))
		return
	}
	switch err := server.ReportManagedDataQuarantined(ctx, task.ProjectName, task.Assignment); {
	case err == nil:
		log.Info("Local Managed data quarantined")
	case errors.Is(err, ErrProjectNotFound):
		log.Info("Project no longer exists; nothing to report")
	default:
		log.Warn("Failed to report quarantine, will retry next poll", zap.Error(err))
	}
}

// reportResourcesRemoved reports the assignment's runtime resources gone, and
// returns whether the obligation may go on to its local data.
func reportResourcesRemoved(ctx context.Context, server cleanupServer, task v1.CleanupTask, log *zap.Logger) bool {
	switch err := server.ReportResourcesRemoved(ctx, task.ProjectName, task.Assignment); {
	case err == nil:
		log.Info("Cleanup obligation resolved")
		return true
	case errors.Is(err, ErrProjectNotFound):
		log.Info("Project no longer exists; nothing to report")
	case errors.Is(err, ErrStaleAssignment):
		// Either the obligation no longer stands, or the write lost its
		// compare-and-swap; only a fresh read tells which. Nothing is lost by
		// guessing wrong here — the obligation, if it stands, is listed again
		// next poll — but the log should say which happened.
		if obligationStands(ctx, server, task.ProjectName, task.Assignment) {
			log.Warn("Cleanup report conflicted and the obligation still stands, will retry next poll", zap.Error(err))
		} else {
			log.Info("Cleanup obligation no longer stands", zap.Error(err))
		}
	default:
		log.Warn("Failed to report cleanup, will retry next poll", zap.Error(err))
	}
	return false
}

// projectReader is the one server call obligationStands needs.
type projectReader interface {
	GetProject(ctx context.Context, name string) (*v1.Project, error)
}

// obligationStands reports whether, on a fresh read, the Project still holds
// an unresolved obligation for assignment. It is how a 409 from the
// resources-removed endpoint is read: the server answers 409 both when there
// is no such obligation and when its compare-and-swap lost every retry, and
// only the second means the report still has to be made.
//
// A read that fails, or a Project that cannot be found, counts as standing:
// the answer is unknown, and treating it as "no longer needed" is the guess
// that can drop a report.
func obligationStands(ctx context.Context, server projectReader, project string, assignment v1.AssignmentIdentity) bool {
	p, err := server.GetProject(ctx, project)
	if err != nil {
		return true
	}
	if p.ObjectMeta.UID != assignment.ProjectUID {
		return false
	}
	o := p.Status.Recovery.FindObligation(assignment)
	return o != nil && !o.Resolved()
}

// revalidateCleanup asks the server, under the claim, whether the task still
// stands. It returns the obligation as it is now, or why not. A listing can be stale by
// the time it is acted on: the obligation resolved, the Project deleted or
// recreated under a new UID, or assigned back to this Node.
//
// An unreachable server is a reason to skip, never to proceed.
func revalidateCleanup(ctx context.Context, server cleanupServer, task v1.CleanupTask) (*v1.CleanupObligation, string) {
	if task.Assignment.NodeName != server.NodeName() {
		return nil, "obligation belongs to node " + task.Assignment.NodeName
	}
	p, err := server.GetProject(ctx, task.ProjectName)
	switch {
	case errors.Is(err, ErrProjectNotFound):
		// The record and its obligations are gone. What is left on this Node
		// is the orphan sweep's to find.
		return nil, "project no longer exists"
	case err != nil:
		return nil, "could not re-read the project: " + err.Error()
	}
	if p.ObjectMeta.UID != task.Assignment.ProjectUID {
		return nil, "project was recreated under another UID"
	}
	if p.Status.NodeRef == server.NodeName() {
		return nil, "this node holds the project's current assignment"
	}
	o := p.Status.Recovery.FindObligation(task.Assignment)
	if o == nil {
		return nil, "obligation no longer recorded"
	}
	if o.Resolved() && !o.DataOutstanding() {
		return nil, "obligation already resolved"
	}
	return o, ""
}
