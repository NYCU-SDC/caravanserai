package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeResource is a CleanupResource whose holdings the test controls.
type fakeResource struct {
	held      []string // what Remaining reports until Remove clears it
	stuck     bool     // Remove succeeds but leaves held in place
	removeErr error
	readErr   error
	removed   []docker.Assignment
}

func (*fakeResource) Kind() string { return "fake" }

func (f *fakeResource) Remove(_ context.Context, a docker.Assignment) error {
	f.removed = append(f.removed, a)
	if f.removeErr != nil {
		return f.removeErr
	}
	if !f.stuck {
		f.held = nil
	}
	return nil
}

func (f *fakeResource) Remaining(context.Context, docker.Assignment) ([]string, error) {
	return f.held, f.readErr
}

// fakeCleanupServer answers the cleanup calls from a fixed Project and
// records the reports it receives.
type fakeCleanupServer struct {
	node      string
	tasks     []v1.CleanupTask
	project   *v1.Project
	getErr    error
	reportErr error
	gets      int
	reported  []v1.AssignmentIdentity
	onGet     func()
}

func (f *fakeCleanupServer) NodeName() string { return f.node }

func (f *fakeCleanupServer) ListCleanupTasks(context.Context) ([]v1.CleanupTask, error) {
	return f.tasks, nil
}

func (f *fakeCleanupServer) GetProject(context.Context, string) (*v1.Project, error) {
	f.gets++
	if f.onGet != nil {
		f.onGet()
	}
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.project, nil
}

func (f *fakeCleanupServer) ReportResourcesRemoved(_ context.Context, _ string, a v1.AssignmentIdentity) error {
	f.reported = append(f.reported, a)
	return f.reportErr
}

var owedAssignment = v1.AssignmentIdentity{ProjectUID: "uid-1", NodeName: "node-a", AssignmentGeneration: 3}

// newCleanupCase is node-a owing cleanup for generation 3 of blog, which now
// runs on node-b.
func newCleanupCase() (*fakeCleanupServer, *fakeResource) {
	server := &fakeCleanupServer{
		node: "node-a",
		tasks: []v1.CleanupTask{{
			Namespace: "default", ProjectName: "blog",
			Assignment: owedAssignment, ReleaseAuthorization: v1.ReleaseByWorkloadStopped,
		}},
		project: &v1.Project{
			ObjectMeta: v1.ObjectMeta{Name: "blog", Namespace: "default", UID: "uid-1"},
			Status: v1.ProjectStatus{
				Phase: v1.ProjectPhaseRunning, NodeRef: "node-b", AssignmentGeneration: 4,
				Recovery: &v1.ProjectRecoveryStatus{CleanupObligations: []v1.CleanupObligation{
					{Assignment: owedAssignment, ReleaseAuthorization: v1.ReleaseByWorkloadStopped},
				}},
			},
		},
	}
	return server, &fakeResource{held: []string{"container /cara-blog-web"}}
}

func TestCleanupTaskResolvesObligation(t *testing.T) {
	server, res := newCleanupCase()
	coordinator := backup.NewCoordinator()

	runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), coordinator, zap.NewNop())

	require.Len(t, res.removed, 1)
	assert.Equal(t, docker.Assignment{Namespace: "default", Project: "blog", UID: "uid-1", Generation: 3}, res.removed[0])
	assert.Equal(t, []v1.AssignmentIdentity{owedAssignment}, server.reported)
	assert.False(t, coordinator.IsBusy(backup.ResourceKey{Namespace: "default", Name: "blog"}), "claim not released")
}

// The listing is acted on only after the server confirms, under the claim,
// that the obligation still stands. Every reason it may not leaves Docker and
// the server untouched.
func TestCleanupTaskRevalidatesBeforeRemoving(t *testing.T) {
	cases := map[string]func(*fakeCleanupServer){
		"project deleted":         func(s *fakeCleanupServer) { s.getErr = ErrProjectNotFound },
		"server unreachable":      func(s *fakeCleanupServer) { s.getErr = errors.New("connection refused") },
		"recreated under new UID": func(s *fakeCleanupServer) { s.project.ObjectMeta.UID = "uid-2" },
		"assigned back here":      func(s *fakeCleanupServer) { s.project.Status.NodeRef = "node-a" },
		"obligation gone":         func(s *fakeCleanupServer) { s.project.Status.Recovery.CleanupObligations = nil },
		"obligation resolved": func(s *fakeCleanupServer) {
			s.project.Status.Recovery.CleanupObligations[0].ResourcesRemoved = &v1.ResourcesRemovedEvidence{}
		},
		// node-b's obligation exists and is unresolved, so only the check
		// that a task is this node's own stops node-a acting on it.
		"another node's task": func(s *fakeCleanupServer) {
			theirs := owedAssignment
			theirs.NodeName = "node-b"
			s.tasks[0].Assignment = theirs
			s.project.Status.NodeRef = "node-c"
			s.project.Status.Recovery.CleanupObligations = append(s.project.Status.Recovery.CleanupObligations,
				v1.CleanupObligation{Assignment: theirs, ReleaseAuthorization: v1.ReleaseByWorkloadStopped})
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			server, res := newCleanupCase()
			change(server)

			runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), backup.NewCoordinator(), zap.NewNop())

			assert.Empty(t, res.removed, "removed without a standing obligation")
			assert.Empty(t, server.reported)
		})
	}
}

// A losing claim skips the task for this poll without even asking the server:
// work must not wait unvalidated behind whatever holds the claim.
func TestCleanupTaskSkipsWhenClaimed(t *testing.T) {
	server, res := newCleanupCase()
	coordinator := backup.NewCoordinator()
	release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: "default", Name: "blog"}, backup.OpRecovery)
	require.True(t, ok)
	defer release()

	runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), coordinator, zap.NewNop())

	assert.Zero(t, server.gets)
	assert.Empty(t, res.removed)
	assert.Empty(t, server.reported)
}

// The claim is held from before the revalidation until after the report, so
// the orphan sweep cannot act on the Project in between.
func TestCleanupTaskHoldsClaimThroughout(t *testing.T) {
	server, res := newCleanupCase()
	coordinator := backup.NewCoordinator()
	key := backup.ResourceKey{Namespace: "default", Name: "blog"}
	var heldDuringGet bool
	server.onGet = func() {
		_, ok := coordinator.TryClaim(key, backup.OpOrphanCleanup)
		heldDuringGet = !ok
	}

	runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), coordinator, zap.NewNop())

	assert.True(t, heldDuringGet, "revalidation ran without the claim")
	assert.Len(t, server.reported, 1)
}

// ResourcesRemoved is reported on a fresh reading that finds nothing, never
// on a removal that merely returned no error.
func TestCleanupTaskReportsOnlyWhenNothingRemains(t *testing.T) {
	t.Run("removal failed", func(t *testing.T) {
		server, res := newCleanupCase()
		res.removeErr = errors.New("daemon busy")
		runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), nil, zap.NewNop())
		assert.Empty(t, server.reported)
	})
	t.Run("something survived the removal", func(t *testing.T) {
		server, res := newCleanupCase()
		res.stuck = true
		runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), nil, zap.NewNop())
		assert.Empty(t, server.reported)
	})
	t.Run("what remains cannot be read", func(t *testing.T) {
		server, res := newCleanupCase()
		res.readErr = errors.New("daemon unreachable")
		runCleanupTasks(context.Background(), server, newAssignmentCleaner(res), nil, zap.NewNop())
		assert.Empty(t, server.reported)
	})
	t.Run("one of several kinds is left", func(t *testing.T) {
		server, done := newCleanupCase()
		left := &fakeResource{held: []string{"certificate file"}, stuck: true}
		runCleanupTasks(context.Background(), server, newAssignmentCleaner(done, left), nil, zap.NewNop())
		assert.Len(t, left.removed, 1, "every registered kind is removed")
		assert.Empty(t, server.reported, "reported while a registered kind still held something")
	})
}

// terminationServer serves one Terminating Project's status and evidence
// endpoints and records the order of the writes it receives. current is what
// GET returns for the Project; nil answers 500.
func terminationServer(t *testing.T, resourcesRemovedStatus int, current *v1.Project) (*Client, *[]string) {
	t.Helper()
	var (
		mu     sync.Mutex
		writes []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/v1/projects/{name}/recovery/resources-removed", func(w http.ResponseWriter, r *http.Request) {
		var req evidenceRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		writes = append(writes, "ResourcesRemoved")
		mu.Unlock()
		w.WriteHeader(resourcesRemovedStatus)
	})
	mux.HandleFunc("GET /api/v1/projects/{name}", func(w http.ResponseWriter, _ *http.Request) {
		if current == nil {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(current)
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var req projectStatusRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		mu.Lock()
		writes = append(writes, string(req.Phase))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return NewClient(zap.NewNop(), server.URL, "test-node"), &writes
}

func teardownRuntime() *mockRuntime {
	return &mockRuntime{removeFn: func(context.Context, string, string, v1.ProjectSpec) error { return nil }}
}

// The Deletion obligation is resolved before Terminated is reported, so a
// Terminated Project never waits on a report that might not come.
func TestTerminateReportsResourcesRemovedBeforeTerminated(t *testing.T) {
	client, writes := terminationServer(t, http.StatusNoContent, nil)
	res := &fakeResource{}

	terminateOne(context.Background(), client, teardownRuntime(), nil, nil, newAssignmentCleaner(res), terminatingProject("blog"), zap.NewNop())

	assert.Equal(t, []string{"ResourcesRemoved", string(v1.ProjectPhaseTerminated)}, *writes)
	require.Len(t, res.removed, 1, "registered kinds are removed during teardown too")
}

func TestTerminateWaitsForResourcesToGo(t *testing.T) {
	client, writes := terminationServer(t, http.StatusNoContent, nil)
	res := &fakeResource{held: []string{"volume cara-blog-cache"}, stuck: true}

	terminateOne(context.Background(), client, teardownRuntime(), nil, nil, newAssignmentCleaner(res), terminatingProject("blog"), zap.NewNop())

	assert.Empty(t, *writes, "reported while resources remained")
}

func TestTerminateRetriesWhenTheReportFails(t *testing.T) {
	client, writes := terminationServer(t, http.StatusInternalServerError, nil)

	terminateOne(context.Background(), client, teardownRuntime(), nil, nil, newAssignmentCleaner(&fakeResource{}), terminatingProject("blog"), zap.NewNop())

	assert.Equal(t, []string{"ResourcesRemoved"}, *writes, "Terminated reported although ResourcesRemoved was not recorded")
}

// owedTermination is blog, Terminating on test-node, with its Deletion
// obligation recorded as resolved or not.
func owedTermination(resolved bool) *v1.Project {
	p := terminatingProject("blog")
	p.ObjectMeta.UID = "uid-1"
	p.Status.AssignmentGeneration = 2
	o := v1.CleanupObligation{
		Assignment:           v1.AssignmentIdentity{ProjectUID: "uid-1", NodeName: "test-node", AssignmentGeneration: 2},
		ReleaseAuthorization: v1.ReleaseByDeletion,
	}
	if resolved {
		o.ResourcesRemoved = &v1.ResourcesRemovedEvidence{}
	}
	p.Status.Recovery = &v1.ProjectRecoveryStatus{CleanupObligations: []v1.CleanupObligation{o}}
	return p
}

// A 409 is read by looking again. With no Deletion obligation, or one already
// resolved, there is nothing a retry would fix and termination proceeds.
func TestTerminateProceedsWithoutAnObligation(t *testing.T) {
	for name, current := range map[string]*v1.Project{
		"none recorded":    terminatingProject("blog"),
		"already resolved": owedTermination(true),
	} {
		t.Run(name, func(t *testing.T) {
			current.ObjectMeta.UID = "uid-1"
			client, writes := terminationServer(t, http.StatusConflict, current)
			p := owedTermination(false)

			terminateOne(context.Background(), client, teardownRuntime(), nil, nil, newAssignmentCleaner(&fakeResource{}), p, zap.NewNop())

			assert.Equal(t, []string{"ResourcesRemoved", string(v1.ProjectPhaseTerminated)}, *writes)
		})
	}
}

// The server also answers 409 when its compare-and-swap lost every retry. The
// obligation is then still unresolved, and reporting Terminated would leave it
// so; termination waits for the next poll instead. A Project that cannot be
// read again is treated the same way.
func TestTerminateRetriesWhenTheConflictLeavesTheObligation(t *testing.T) {
	for name, current := range map[string]*v1.Project{
		"obligation still unresolved": owedTermination(false),
		"project cannot be re-read":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			client, writes := terminationServer(t, http.StatusConflict, current)

			terminateOne(context.Background(), client, teardownRuntime(), nil, nil, newAssignmentCleaner(&fakeResource{}), owedTermination(false), zap.NewNop())

			assert.Equal(t, []string{"ResourcesRemoved"}, *writes, "Terminated reported with the deletion obligation unresolved")
		})
	}
}
