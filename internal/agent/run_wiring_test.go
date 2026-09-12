package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// These tests drive the real Run loop, not its parts. Every other recovery
// test calls healthCheckOne or recoverLocally directly and hands it a tracker
// and a coordinator; what they cannot show is that Run builds those once and
// passes the same ones to bootstrap and to every poll. A Run that passed nil
// would silently disable recovery; one that built a tracker per tick would
// retry without backoff forever. Both would pass every other test.

const wiringPoll = 20 * time.Millisecond

// wiringServer is a minimal control plane for Run: registration, heartbeats,
// the assignment list, the re-read, and status and condition writes. It is
// safe for concurrent use because Run calls it from its own goroutine.
type wiringServer struct {
	mu       sync.Mutex
	project  v1.Project
	lists    int
	statuses []statusUpdate
}

func newWiringServer(t *testing.T, p *v1.Project) (*wiringServer, *Client) {
	t.Helper()
	ws := &wiringServer{project: *p}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("POST /api/v1/nodes/{name}/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/v1/projects", func(w http.ResponseWriter, _ *http.Request) {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		ws.lists++
		_ = json.NewEncoder(w).Encode(v1.ProjectList{Items: []v1.Project{ws.project}})
	})
	mux.HandleFunc("GET /api/v1/projects/{name}", func(w http.ResponseWriter, _ *http.Request) {
		ws.mu.Lock()
		defer ws.mu.Unlock()
		_ = json.NewEncoder(w).Encode(ws.project)
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var req projectStatusRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		ws.mu.Lock()
		defer ws.mu.Unlock()
		ws.statuses = append(ws.statuses, statusUpdate{
			ProjectName: r.PathValue("name"), Phase: req.Phase, Reason: req.Reason, Message: req.Message,
		})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/conditions/{type}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v1/projects/{name}/conditions/{type}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return ws, NewClient(zap.NewNop(), srv.URL, gateNode)
}

func (ws *wiringServer) listCount() int {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	return ws.lists
}

func (ws *wiringServer) failed() []statusUpdate {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	var out []statusUpdate
	for _, u := range ws.statuses {
		if u.Phase == v1.ProjectPhaseFailed {
			out = append(out, u)
		}
	}
	return out
}

// wiringRuntime reports a scripted container state and records recovery
// calls. Its state is guarded because Run reads it from another goroutine.
type wiringRuntime struct {
	mockRuntime

	mu     sync.Mutex
	status string // what InspectProject reports for "web"
	healOn bool   // RecoverServices makes "web" running
	calls  [][]string
	onCall func() // runs inside RecoverServices, under no lock
}

func newWiringRuntime(status string, healOn bool) *wiringRuntime {
	r := &wiringRuntime{status: status, healOn: healOn}
	r.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		exit := 0
		if r.status == "exited" {
			exit = 1
		}
		return []docker.ContainerState{{ServiceName: "web", ContainerID: "id-web", Status: r.status, ExitCode: exit}}, nil
	}
	r.recoverFn = func(_ context.Context, _ *v1.Project, services []string) error {
		r.mu.Lock()
		hook := r.onCall
		r.mu.Unlock()
		if hook != nil {
			hook()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, services)
		if r.healOn {
			r.status = "running"
		}
		return nil
	}
	return r
}

func (r *wiringRuntime) setStatus(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status = s
}

func (r *wiringRuntime) recoveries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// startRun runs the real agent loop until the test ends.
func startRun(t *testing.T, client *Client, rt docker.Runtime, coord *backup.Coordinator) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(ctx, RunConfig{
			Client:            client,
			Runtime:           rt,
			HeartbeatInterval: time.Hour,
			PollInterval:      wiringPoll,
			Backups:           &BackupSupport{Coordinator: coord, DataRoot: t.TempDir()},
			Logger:            zap.NewNop(),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancellation")
		}
	})
}

// Bootstrap recovers a Project found broken at startup, through the same
// Coordinator the rest of the agent uses.
func TestRunRecoversAtBootstrapUnderTheAgentsCoordinator(t *testing.T) {
	_, client := newWiringServer(t, gateProject())
	rt := newWiringRuntime("exited", true)
	coord := backup.NewCoordinator()
	rkey := backup.ResourceKey{Namespace: "default", Name: "guestbook"}

	var heldBy atomic.Value
	rt.onCall = func() {
		op, _ := coord.Current(rkey)
		heldBy.Store(op)
	}

	startRun(t, client, rt, coord)

	require.Eventually(t, func() bool { return rt.recoveries() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"Run never recovered the Project")
	assert.Equal(t, backup.OpRecovery, heldBy.Load(),
		"recovery must claim the Coordinator Run was given, not a private one")
}

// The tracker is built once and shared by bootstrap and every poll. A
// container that stays down is attempted once at bootstrap and then left for
// the 5s backoff, across many polls — which is only true if every poll sees
// the same tracker. A nil tracker would report Failed at once; a fresh one per
// poll would attempt on every tick.
func TestRunSharesOneTrackerAcrossBootstrapAndPolls(t *testing.T) {
	ws, client := newWiringServer(t, gateProject())
	rt := newWiringRuntime("exited", false) // recovery does not fix it
	startRun(t, client, rt, backup.NewCoordinator())

	require.Eventually(t, func() bool { return rt.recoveries() >= 1 }, 2*time.Second, 5*time.Millisecond)
	start := ws.listCount()
	require.Eventually(t, func() bool { return ws.listCount() >= start+10 }, 2*time.Second, 5*time.Millisecond,
		"the poll loop should have run many times")

	assert.Equal(t, 1, rt.recoveries(), "polls inside the backoff must not attempt again")
	assert.Empty(t, ws.failed(), "a Project inside its recovery budget is not Failed")
}

// The poll path, not only bootstrap: a Project healthy at startup that breaks
// later is recovered by a poll, under the same Coordinator.
func TestRunRecoversAProjectThatBreaksAfterStartup(t *testing.T) {
	ws, client := newWiringServer(t, gateProject())
	rt := newWiringRuntime("running", true)
	coord := backup.NewCoordinator()
	rkey := backup.ResourceKey{Namespace: "default", Name: "guestbook"}

	var heldBy atomic.Value
	rt.onCall = func() {
		op, _ := coord.Current(rkey)
		heldBy.Store(op)
	}

	startRun(t, client, rt, coord)

	// Let bootstrap and a few polls see it healthy.
	start := ws.listCount()
	require.Eventually(t, func() bool { return ws.listCount() >= start+3 }, 2*time.Second, 5*time.Millisecond)
	require.Zero(t, rt.recoveries())

	rt.setStatus("exited")

	require.Eventually(t, func() bool { return rt.recoveries() >= 1 }, 2*time.Second, 5*time.Millisecond,
		"a poll should have recovered the Project")
	assert.Equal(t, backup.OpRecovery, heldBy.Load())
	assert.Empty(t, ws.failed())
}

// Backup and recovery never overlap. A backup goroutine repeatedly claims the
// Project and holds it, while the real loop polls and tries to recover. At no
// point may RecoverServices run while the backup's claim is held. Run with
// -race, this also checks the tracker and Coordinator for data races under
// the loop's real concurrency.
func TestRunNeverRecoversWhileABackupHoldsTheProject(t *testing.T) {
	_, client := newWiringServer(t, gateProject())
	rt := newWiringRuntime("exited", false)
	coord := backup.NewCoordinator()
	rkey := backup.ResourceKey{Namespace: "default", Name: "guestbook"}

	var backupHolding atomic.Bool
	var overlaps atomic.Int32
	rt.onCall = func() {
		if backupHolding.Load() {
			overlaps.Add(1)
		}
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if release, ok := coord.TryClaim(rkey, backup.OpBackup); ok {
				backupHolding.Store(true)
				time.Sleep(3 * time.Millisecond)
				backupHolding.Store(false)
				release()
			}
			time.Sleep(time.Millisecond)
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })

	startRun(t, client, rt, coord)
	time.Sleep(40 * wiringPoll)

	require.GreaterOrEqual(t, rt.recoveries(), 1,
		"recovery must actually have run, or the absence of overlap proves nothing")
	assert.Zero(t, overlaps.Load(), "RecoverServices ran while a backup held the Project")
}
