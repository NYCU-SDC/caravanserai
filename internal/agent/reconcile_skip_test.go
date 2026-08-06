package agent

import (
	"context"
	"encoding/json"
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

// newReconcileServer serves a fixed project list and records status writes,
// so a reconcile pass can be observed end to end.
func newReconcileServer(t *testing.T, projects []v1.Project) (*Client, *[]statusUpdate) {
	t.Helper()

	var (
		mu      sync.Mutex
		updates []statusUpdate
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v1.ProjectList{Items: projects})
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var req projectStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		updates = append(updates, statusUpdate{
			ProjectName: r.PathValue("name"),
			Phase:       req.Phase,
			Reason:      req.Reason,
			Message:     req.Message,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return NewClient(zap.NewNop(), server.URL, "test-node"), &updates
}

func runningProject(name string) v1.Project {
	return v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning, NodeRef: "test-node"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
		},
	}
}

func TestReconcileProjectsSkipsBusyProject(t *testing.T) {
	// The guarantee this protects: while a backup holds a Project its
	// containers are stopped, so a health check would report Failed. That
	// false Failed both misrepresents a healthy service and unlocks the
	// apply/delete paths that are closed while a Project is Running.
	client, updates := newReconcileServer(t, []v1.Project{runningProject("blog")})

	coordinator := backup.NewCoordinator()
	release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: "default", Name: "blog"}, backup.OpBackup)
	require.True(t, ok)
	defer release()

	// No containers exist, which without the skip would be reported Failed.
	rt := &mockRuntime{
		inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			return nil, nil
		},
	}

	reconcileProjects(context.Background(), client, rt, nil, &BackupSupport{Coordinator: coordinator}, nil, zap.NewNop())

	assert.Empty(t, *updates, "a Project with an operation in flight must produce no status write")
}

func TestReconcileProjectsProcessesUnclaimedProject(t *testing.T) {
	// The mirror of the skip test: without a claim, the same Project is
	// health-checked and reported Failed as usual.
	client, updates := newReconcileServer(t, []v1.Project{runningProject("blog")})

	coordinator := backup.NewCoordinator()
	rt := &mockRuntime{
		inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			return nil, nil
		},
	}

	reconcileProjects(context.Background(), client, rt, nil, &BackupSupport{Coordinator: coordinator}, nil, zap.NewNop())

	require.Len(t, *updates, 1)
	assert.Equal(t, v1.ProjectPhaseFailed, (*updates)[0].Phase)
}

func TestReconcileProjectsSkipsOnlyTheBusyProject(t *testing.T) {
	client, updates := newReconcileServer(t, []v1.Project{
		runningProject("blog"),
		runningProject("wiki"),
	})

	coordinator := backup.NewCoordinator()
	release, ok := coordinator.TryClaim(backup.ResourceKey{Namespace: "default", Name: "blog"}, backup.OpBackup)
	require.True(t, ok)
	defer release()

	rt := &mockRuntime{
		inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			return nil, nil
		},
	}

	reconcileProjects(context.Background(), client, rt, nil, &BackupSupport{Coordinator: coordinator}, nil, zap.NewNop())

	require.Len(t, *updates, 1, "only the unclaimed Project is processed")
	assert.Equal(t, "wiki", (*updates)[0].ProjectName)
}

// ── terminateOne / coordinator ──────────────────────────────────────────────

func terminatingProject(name string) *v1.Project {
	return &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseTerminating, NodeRef: "test-node"},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
		},
	}
}

func TestTerminateOneClaimsAndReleases(t *testing.T) {
	// terminateOne must claim OpTerminate before touching Docker so a backup
	// tick cannot start mid-teardown, and must release the claim on every exit
	// path so a stuck claim never blocks the Project forever.
	client, updates := newReconcileServer(t, nil)
	coordinator := backup.NewCoordinator()

	var removed bool
	rt := &mockRuntime{
		removeFn: func(context.Context, string, string, v1.ProjectSpec) error {
			removed = true
			return nil
		},
	}

	p := terminatingProject("blog")
	terminateOne(context.Background(), client, rt, nil, coordinator, p, zap.NewNop())

	assert.True(t, removed, "RemoveProject must be called when the claim succeeds")
	require.Len(t, *updates, 1)
	assert.Equal(t, v1.ProjectPhaseTerminated, (*updates)[0].Phase)

	key := backup.ResourceKey{Namespace: "default", Name: "blog"}
	assert.False(t, coordinator.IsBusy(key), "the claim must be released after terminateOne returns")
}

func TestTerminateOneSkipsWhenAlreadyClaimed(t *testing.T) {
	// Simulates a backup winning the race between reconcileProjects' IsBusy
	// check and terminateOne's own TryClaim: teardown must not proceed, and
	// nothing Docker-side or status-side should happen this tick.
	client, updates := newReconcileServer(t, nil)
	coordinator := backup.NewCoordinator()

	key := backup.ResourceKey{Namespace: "default", Name: "blog"}
	release, ok := coordinator.TryClaim(key, backup.OpBackup)
	require.True(t, ok)
	defer release()

	var removed bool
	rt := &mockRuntime{
		removeFn: func(context.Context, string, string, v1.ProjectSpec) error {
			removed = true
			return nil
		},
	}

	p := terminatingProject("blog")
	terminateOne(context.Background(), client, rt, nil, coordinator, p, zap.NewNop())

	assert.False(t, removed, "RemoveProject must not run while another operation holds the claim")
	assert.Empty(t, *updates, "no status write when termination was deferred")
}

func TestReconcileProjectsWithoutBackupSupport(t *testing.T) {
	// Backups are optional; with no coordinator the loop must behave exactly
	// as it did before they existed.
	client, updates := newReconcileServer(t, []v1.Project{runningProject("blog")})

	rt := &mockRuntime{
		inspectFn: func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
			return nil, nil
		},
	}

	reconcileProjects(context.Background(), client, rt, nil, nil, nil, zap.NewNop())

	require.Len(t, *updates, 1)
	assert.Equal(t, v1.ProjectPhaseFailed, (*updates)[0].Phase)
}
