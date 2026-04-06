package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mocks ────────────────────────────────────────────────────────────────────

// mockRuntime implements docker.Runtime for testing.
type mockRuntime struct {
	inspectFn       func(ctx context.Context, project *v1.Project) ([]docker.ContainerState, error)
	reconcileFn     func(ctx context.Context, project *v1.Project) error
	removeFn        func(ctx context.Context, name string, spec v1.ProjectSpec) error
	getContainerIPs func(ctx context.Context, project *v1.Project) (map[string]string, error)
}

func (m *mockRuntime) InspectProject(ctx context.Context, project *v1.Project) ([]docker.ContainerState, error) {
	if m.inspectFn != nil {
		return m.inspectFn(ctx, project)
	}
	return nil, nil
}

func (m *mockRuntime) ReconcileProject(ctx context.Context, project *v1.Project) error {
	if m.reconcileFn != nil {
		return m.reconcileFn(ctx, project)
	}
	return nil
}

func (m *mockRuntime) RemoveProject(ctx context.Context, name string, spec v1.ProjectSpec) error {
	if m.removeFn != nil {
		return m.removeFn(ctx, name, spec)
	}
	return nil
}

func (m *mockRuntime) GetContainerIPs(ctx context.Context, project *v1.Project) (map[string]string, error) {
	if m.getContainerIPs != nil {
		return m.getContainerIPs(ctx, project)
	}
	return nil, nil
}

// statusUpdate records a single call to PATCH /api/v1/projects/{name}/status.
type statusUpdate struct {
	ProjectName string
	Phase       v1.ProjectPhase
	Reason      string
	Message     string
}

// newTestServer creates an httptest.Server that records status update calls and
// returns a Client wired to it. The returned slice accumulates all status
// patches received.
func newTestServer(t *testing.T) (*httptest.Server, *Client, *[]statusUpdate) {
	t.Helper()

	var (
		mu      sync.Mutex
		updates []statusUpdate
	)

	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var req projectStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		updates = append(updates, statusUpdate{
			ProjectName: name,
			Phase:       req.Phase,
			Reason:      req.Reason,
			Message:     req.Message,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	client := NewClient(zap.NewNop(), server.URL, "test-node")
	return server, client, &updates
}

// ── healthCheckOne tests ─────────────────────────────────────────────────────

func TestHealthCheckOne(t *testing.T) {
	twoServiceProject := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "my-app"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{
				{Name: "web", Image: "nginx:latest"},
				{Name: "db", Image: "postgres:15"},
			},
		},
	}

	tests := []struct {
		name      string
		project   *v1.Project
		states    []docker.ContainerState
		wantCount int // number of status updates expected
		wantPhase v1.ProjectPhase
		reason    string
	}{
		{
			name:    "healthy — all containers running",
			project: twoServiceProject,
			states: []docker.ContainerState{
				{ServiceName: "web", ContainerID: "abc", Status: "running", ExitCode: 0},
				{ServiceName: "db", ContainerID: "def", Status: "running", ExitCode: 0},
			},
			wantCount: 0, // no status update — healthy is a no-op
		},
		{
			name:    "crashed — non-zero exit code",
			project: twoServiceProject,
			states: []docker.ContainerState{
				{ServiceName: "web", ContainerID: "abc", Status: "running", ExitCode: 0},
				{ServiceName: "db", ContainerID: "def", Status: "exited", ExitCode: 137},
			},
			wantCount: 1,
			wantPhase: v1.ProjectPhaseFailed,
			reason:    "ContainerCrashed",
		},
		{
			name:    "missing — fewer containers than services",
			project: twoServiceProject,
			states: []docker.ContainerState{
				{ServiceName: "web", ContainerID: "abc", Status: "running", ExitCode: 0},
				// "db" container is missing
			},
			wantCount: 1,
			wantPhase: v1.ProjectPhaseFailed,
			reason:    "ContainerMissing",
		},
		{
			name:    "exited cleanly — exit code 0",
			project: twoServiceProject,
			states: []docker.ContainerState{
				{ServiceName: "web", ContainerID: "abc", Status: "running", ExitCode: 0},
				{ServiceName: "db", ContainerID: "def", Status: "exited", ExitCode: 0},
			},
			wantCount: 1,
			wantPhase: v1.ProjectPhaseFailed,
			reason:    "ContainerExited",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, client, updates := newTestServer(t)

			rt := &mockRuntime{
				inspectFn: func(_ context.Context, _ *v1.Project) ([]docker.ContainerState, error) {
					return tt.states, nil
				},
			}

			healthCheckOne(context.Background(), client, rt, nil, tt.project, zap.NewNop())

			require.Len(t, *updates, tt.wantCount)
			if tt.wantCount > 0 {
				u := (*updates)[0]
				assert.Equal(t, tt.project.Name, u.ProjectName)
				assert.Equal(t, tt.wantPhase, u.Phase)
				assert.Equal(t, tt.reason, u.Reason)
				assert.NotEmpty(t, u.Message)
			}
		})
	}
}

func TestHealthCheckOne_InspectError(t *testing.T) {
	_, client, updates := newTestServer(t)

	rt := &mockRuntime{
		inspectFn: func(_ context.Context, _ *v1.Project) ([]docker.ContainerState, error) {
			return nil, assert.AnError
		},
	}

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "broken-app"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{{Name: "web", Image: "nginx:latest"}},
		},
	}

	healthCheckOne(context.Background(), client, rt, nil, project, zap.NewNop())

	require.Len(t, *updates, 1)
	u := (*updates)[0]
	assert.Equal(t, "broken-app", u.ProjectName)
	assert.Equal(t, v1.ProjectPhaseFailed, u.Phase)
	assert.Equal(t, "InspectError", u.Reason)
}

func TestHealthCheckOne_CrashedBeforeMissing(t *testing.T) {
	// When both a crashed container and a missing container exist, the crash
	// takes priority (checked first in the function).
	_, client, updates := newTestServer(t)

	rt := &mockRuntime{
		inspectFn: func(_ context.Context, _ *v1.Project) ([]docker.ContainerState, error) {
			return []docker.ContainerState{
				{ServiceName: "web", ContainerID: "abc", Status: "exited", ExitCode: 1},
				// "db" is missing — but crash should take priority
			}, nil
		},
	}

	project := &v1.Project{
		ObjectMeta: v1.ObjectMeta{Name: "multi-fail"},
		Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
		Spec: v1.ProjectSpec{
			Services: []v1.ServiceDef{
				{Name: "web", Image: "nginx:latest"},
				{Name: "db", Image: "postgres:15"},
			},
		},
	}

	healthCheckOne(context.Background(), client, rt, nil, project, zap.NewNop())

	require.Len(t, *updates, 1)
	u := (*updates)[0]
	assert.Equal(t, v1.ProjectPhaseFailed, u.Phase)
	assert.Equal(t, "ContainerCrashed", u.Reason, "crash should take priority over missing")
}

// ── bootstrapRunningProjects test ────────────────────────────────────────────

func TestBootstrapRunningProjects(t *testing.T) {
	// Set up a test server that serves both ListProjectsForReconcile and
	// status update endpoints.
	var (
		mu      sync.Mutex
		updates []statusUpdate
	)

	mux := http.NewServeMux()

	// Return a mix of Running and Scheduled projects.
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		list := v1.ProjectList{
			Items: []v1.Project{
				{
					ObjectMeta: v1.ObjectMeta{Name: "running-healthy"},
					Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
					Spec: v1.ProjectSpec{
						Services: []v1.ServiceDef{{Name: "web", Image: "nginx"}},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{Name: "running-crashed"},
					Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseRunning},
					Spec: v1.ProjectSpec{
						Services: []v1.ServiceDef{{Name: "api", Image: "myapi"}},
					},
				},
				{
					ObjectMeta: v1.ObjectMeta{Name: "scheduled-new"},
					Status:     v1.ProjectStatus{Phase: v1.ProjectPhaseScheduled},
					Spec: v1.ProjectSpec{
						Services: []v1.ServiceDef{{Name: "app", Image: "myapp"}},
					},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(list)
	})

	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		var req projectStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		updates = append(updates, statusUpdate{
			ProjectName: name,
			Phase:       req.Phase,
			Reason:      req.Reason,
			Message:     req.Message,
		})
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	client := NewClient(zap.NewNop(), server.URL, "test-node")

	// The runtime: "running-healthy" has a running container, "running-crashed"
	// has a crashed container.
	rt := &mockRuntime{
		inspectFn: func(_ context.Context, p *v1.Project) ([]docker.ContainerState, error) {
			switch p.Name {
			case "running-healthy":
				return []docker.ContainerState{
					{ServiceName: "web", ContainerID: "c1", Status: "running"},
				}, nil
			case "running-crashed":
				return []docker.ContainerState{
					{ServiceName: "api", ContainerID: "c2", Status: "exited", ExitCode: 1},
				}, nil
			default:
				return nil, nil
			}
		},
	}

	bootstrapRunningProjects(context.Background(), client, rt, nil, zap.NewNop())

	// Only "running-crashed" should have generated a status update (Failed).
	// "running-healthy" is healthy (no-op). "scheduled-new" is skipped
	// because it's not Running.
	require.Len(t, updates, 1)
	assert.Equal(t, "running-crashed", updates[0].ProjectName)
	assert.Equal(t, v1.ProjectPhaseFailed, updates[0].Phase)
	assert.Equal(t, "ContainerCrashed", updates[0].Reason)
}
