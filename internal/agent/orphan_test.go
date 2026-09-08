package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type fakeClock struct {
	now time.Time
}

func (fc *fakeClock) Now() time.Time                  { return fc.now }
func (fc *fakeClock) Since(t time.Time) time.Duration { return fc.now.Sub(t) }
func (fc *fakeClock) advance(d time.Duration)         { fc.now = fc.now.Add(d) }

type recordingRoutes struct {
	mu      sync.Mutex
	removed []string
	updated []string
}

func (r *recordingRoutes) Update(project *v1.Project, _ map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updated = append(r.updated, project.Name)
}

func (r *recordingRoutes) Remove(projectName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.removed = append(r.removed, projectName)
}

func (r *recordingRoutes) removedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.removed...)
}

func (r *recordingRoutes) updatedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.updated...)
}

type sweepRecord struct {
	local      []docker.ProjectIdentity
	listErr    error
	stopErr    error
	removeErr  error
	stopped    []docker.ProjectIdentity
	removed    []docker.ProjectIdentity
	reconciled []docker.ProjectIdentity
}

func newSweepFixture(local ...docker.ProjectIdentity) (*mockRuntime, *sweepRecord) {
	record := &sweepRecord{local: append([]docker.ProjectIdentity(nil), local...)}
	runtime := &mockRuntime{
		listLocalFn: func(context.Context) ([]docker.ProjectIdentity, error) {
			return append([]docker.ProjectIdentity(nil), record.local...), record.listErr
		},
		stopOrphanFn: func(_ context.Context, project docker.ProjectIdentity) error {
			record.stopped = append(record.stopped, project)
			return record.stopErr
		},
		removeOrphanFn: func(_ context.Context, project docker.ProjectIdentity) error {
			record.removed = append(record.removed, project)
			return record.removeErr
		},
		reconcileFn: func(_ context.Context, project *v1.Project) error {
			record.reconciled = append(record.reconciled, projectIdentity(project, false))
			return nil
		},
		getContainerIPs: func(context.Context, *v1.Project) (map[string]string, error) {
			return map[string]string{"web": "172.18.0.2"}, nil
		},
	}
	return runtime, record
}

func identity(namespace, name string) docker.ProjectIdentity {
	return docker.ProjectIdentity{Namespace: namespace, Name: name}
}

func assignedProject(namespace, name string, phase v1.ProjectPhase) *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Namespace: namespace, Name: name},
		Status:     v1.ProjectStatus{Phase: phase},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
			Ingress: []v1.IngressDef{{
				Name:   "web",
				Target: v1.IngressTarget{Service: "web", Port: 80},
			}},
		},
	}
}

func sweep(
	runtime docker.Runtime,
	routes RouteUpdater,
	tracker *orphanTracker,
	busy busyChecker,
	projects ...*v1.Project,
) {
	sweepOrphans(context.Background(), runtime, routes, tracker, busy, projects, false, zap.NewNop())
}

func TestSweepOrphans_StopsImmediatelyThenRemovesAfterGrace(t *testing.T) {
	gone := identity("default", "gone")
	kept := identity("default", "kept")
	runtime, record := newSweepFixture(gone, kept)
	routes := &recordingRoutes{}
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	sweep(runtime, routes, tracker, nil, assignedProject("default", "kept", v1.ProjectPhaseRunning))
	require.Equal(t, []docker.ProjectIdentity{gone}, record.stopped)
	assert.Empty(t, record.removed)
	assert.Equal(t, []string{"gone"}, routes.removedNames())

	clock.advance(orphanGracePeriod - time.Second)
	sweep(runtime, routes, tracker, nil, assignedProject("default", "kept", v1.ProjectPhaseRunning))
	assert.Empty(t, record.removed, "resources must survive until the full grace period")

	clock.advance(time.Second)
	sweep(runtime, routes, tracker, nil, assignedProject("default", "kept", v1.ProjectPhaseRunning))
	assert.Equal(t, []docker.ProjectIdentity{gone}, record.removed)
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_FailedProjectStillAssignedIsUntouched(t *testing.T) {
	failed := identity("default", "failed")
	runtime, record := newSweepFixture(failed)
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)
	assigned := assignedProject("default", "failed", v1.ProjectPhaseFailed)

	sweep(runtime, nil, tracker, nil, assigned)
	clock.advance(2 * orphanGracePeriod)
	sweep(runtime, nil, tracker, nil, assigned)

	assert.Empty(t, record.stopped)
	assert.Empty(t, record.removed)
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_NamespaceIsPartOfOwnership(t *testing.T) {
	local := identity("team-a", "same-name")
	runtime, record := newSweepFixture(local)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	sweep(runtime, nil, tracker, nil,
		assignedProject("team-b", "same-name", v1.ProjectPhaseRunning))

	assert.Equal(t, []docker.ProjectIdentity{local}, record.stopped)
}

func TestSweepOrphans_RunningOwnershipReturnRecoversWorkloadAndRoutes(t *testing.T) {
	project := identity("default", "returning")
	runtime, record := newSweepFixture(project)
	routes := &recordingRoutes{}
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	sweep(runtime, routes, tracker, nil)
	require.Equal(t, []docker.ProjectIdentity{project}, record.stopped)

	clock.advance(time.Minute)
	sweep(runtime, routes, tracker, nil,
		assignedProject("default", "returning", v1.ProjectPhaseRunning))

	assert.Equal(t, []docker.ProjectIdentity{project}, record.reconciled)
	assert.Equal(t, []string{"returning"}, routes.updatedNames())
	assert.Empty(t, record.removed)
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_FailedOwnershipReturnDoesNotStartWorkload(t *testing.T) {
	project := identity("default", "failed-return")
	runtime, record := newSweepFixture(project)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	sweep(runtime, nil, tracker, nil)
	sweep(runtime, nil, tracker, nil,
		assignedProject("default", "failed-return", v1.ProjectPhaseFailed))

	assert.Empty(t, record.reconciled)
	assert.Empty(t, record.removed)
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_DockerListErrorResetsDeletionGrace(t *testing.T) {
	project := identity("default", "docker-blip")
	runtime, record := newSweepFixture(project)
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	sweep(runtime, nil, tracker, nil)
	clock.advance(2 * time.Minute)
	record.listErr = errors.New("docker unavailable")
	sweep(runtime, nil, tracker, nil)
	record.listErr = nil

	// The first known-absent observation after an error starts a fresh clock.
	sweep(runtime, nil, tracker, nil)
	clock.advance(orphanGracePeriod - time.Second)
	sweep(runtime, nil, tracker, nil)
	assert.Empty(t, record.removed)

	clock.advance(time.Second)
	sweep(runtime, nil, tracker, nil)
	assert.Equal(t, []docker.ProjectIdentity{project}, record.removed)
}

func TestSweepOrphans_StopFailureRetriesWithoutRemovingRoute(t *testing.T) {
	project := identity("default", "stop-fails")
	runtime, record := newSweepFixture(project)
	record.stopErr = errors.New("docker refused")
	routes := &recordingRoutes{}
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	sweep(runtime, routes, tracker, nil)
	clock.advance(2 * orphanGracePeriod)
	sweep(runtime, routes, tracker, nil)

	assert.Len(t, record.stopped, 2)
	assert.Empty(t, record.removed)
	assert.Empty(t, routes.removedNames())
	assert.Equal(t, 1, tracker.pending())
}

func TestSweepOrphans_RemoveFailureRetriesAfterContainersDisappear(t *testing.T) {
	project := identity("default", "remove-fails")
	runtime, record := newSweepFixture(project)
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)

	sweep(runtime, nil, tracker, nil)
	clock.advance(orphanGracePeriod)
	record.removeErr = errors.New("network busy")
	sweep(runtime, nil, tracker, nil)
	require.Len(t, record.removed, 1)

	// Container deletion may have succeeded before network deletion failed.
	// The tracker must retain the identity and retry the remaining resources.
	record.local = nil
	record.removeErr = nil
	sweep(runtime, nil, tracker, nil)
	assert.Len(t, record.removed, 2)
	assert.Equal(t, 0, tracker.pending())
}

func TestSweepOrphans_BusyProjectIsNotStopped(t *testing.T) {
	project := identity("default", "backing-up")
	runtime, record := newSweepFixture(project)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})
	coordinator := backup.NewCoordinator()
	release, ok := coordinator.TryClaim(
		backup.ResourceKey{Namespace: project.Namespace, Name: project.Name},
		backup.OpBackup,
	)
	require.True(t, ok)

	sweep(runtime, nil, tracker, coordinator)
	assert.Empty(t, record.stopped)
	release()

	sweep(runtime, nil, tracker, coordinator)
	assert.Equal(t, []docker.ProjectIdentity{project}, record.stopped)
}

func TestReconcileProjects_ServerErrorResetsDeletionGrace(t *testing.T) {
	project := identity("default", "server-blip")
	runtime, record := newSweepFixture(project)
	clock := &fakeClock{now: time.Now()}
	tracker := newOrphanTracker(clock)
	sweep(runtime, nil, tracker, nil)

	clock.advance(2 * time.Minute)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := NewClient(zap.NewNop(), server.URL, "test-node")
	reconcileProjects(context.Background(), client, runtime, nil, nil, tracker, false, zap.NewNop())

	// A fresh successful snapshot starts a new full grace period.
	sweep(runtime, nil, tracker, nil)
	clock.advance(orphanGracePeriod - time.Second)
	sweep(runtime, nil, tracker, nil)
	assert.Empty(t, record.removed)

	clock.advance(time.Second)
	sweep(runtime, nil, tracker, nil)
	assert.Equal(t, []docker.ProjectIdentity{project}, record.removed)
}

// identityUID builds a ProjectIdentity carrying a UID, mirroring what
// DockerRuntime.ListLocalProjects emits when UID enforcement is on.
func identityUID(namespace, name, uid string) docker.ProjectIdentity {
	return docker.ProjectIdentity{Namespace: namespace, Name: name, UID: uid}
}

func assignedProjectUID(namespace, name, uid string, phase v1.ProjectPhase) *v1.Project {
	p := assignedProject(namespace, name, phase)
	p.ObjectMeta.UID = uid
	return p
}

// With UID enforcement on, a leftover container from a previous lifetime of the
// same (namespace, name) — a different UID — is not owned by the current
// assignment and must be quarantined (stopped), even though the server still
// assigns a Project with that name to this node (CARA-82 delete-and-recreate).
func TestSweepOrphans_UIDEnforcement_QuarantinesPreviousLifetime(t *testing.T) {
	oldLifetime := identityUID("default", "foo", "uid-old")
	runtime, record := newSweepFixture(oldLifetime)
	routes := &recordingRoutes{}
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	// Server assigns a freshly recreated "foo" with a different UID.
	current := assignedProjectUID("default", "foo", "uid-new", v1.ProjectPhaseRunning)
	sweepOrphans(context.Background(), runtime, routes, tracker, nil,
		[]*v1.Project{current}, true, zap.NewNop())

	assert.Equal(t, []docker.ProjectIdentity{oldLifetime}, record.stopped,
		"previous-lifetime container must be quarantined, not adopted as the new Project")
	assert.Empty(t, record.reconciled, "the stale container must never be reconciled as current")
}

// With UID enforcement on, a pre-UID (legacy) container carries no UID label and
// therefore cannot be proven to belong to the current assignment; it is
// quarantined rather than silently adopted.
func TestSweepOrphans_UIDEnforcement_QuarantinesLegacyContainer(t *testing.T) {
	legacy := identityUID("default", "foo", "") // no UID label
	runtime, record := newSweepFixture(legacy)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	current := assignedProjectUID("default", "foo", "uid-new", v1.ProjectPhaseRunning)
	sweepOrphans(context.Background(), runtime, &recordingRoutes{}, tracker, nil,
		[]*v1.Project{current}, true, zap.NewNop())

	assert.Equal(t, []docker.ProjectIdentity{legacy}, record.stopped,
		"legacy container with no UID must be quarantined under enforcement")
}

// A container whose UID matches the current assignment is owned and left alone.
func TestSweepOrphans_UIDEnforcement_KeepsMatchingUID(t *testing.T) {
	current := identityUID("default", "foo", "uid-new")
	runtime, record := newSweepFixture(current)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	assigned := assignedProjectUID("default", "foo", "uid-new", v1.ProjectPhaseRunning)
	sweepOrphans(context.Background(), runtime, &recordingRoutes{}, tracker, nil,
		[]*v1.Project{assigned}, true, zap.NewNop())

	assert.Empty(t, record.stopped, "a container with the matching UID must not be quarantined")
}

// In compatibility mode (enforcement off) a same-name container is matched by
// (namespace, name) regardless of UID, preserving pre-CARA-82 behaviour so a
// single upgraded agent does not quarantine still-valid containers.
func TestSweepOrphans_CompatibilityMode_IgnoresUID(t *testing.T) {
	legacy := identity("default", "foo") // ListLocalProjects emits no UID when off
	runtime, record := newSweepFixture(legacy)
	tracker := newOrphanTracker(&fakeClock{now: time.Now()})

	current := assignedProjectUID("default", "foo", "uid-new", v1.ProjectPhaseRunning)
	sweepOrphans(context.Background(), runtime, &recordingRoutes{}, tracker, nil,
		[]*v1.Project{current}, false, zap.NewNop())

	assert.Empty(t, record.stopped, "compatibility mode must match by name and keep the container")
}
