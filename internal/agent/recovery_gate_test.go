package agent

import (
	"context"
	"encoding/json"
	"fmt"
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

// These tests pin down the order recoverLocally runs its gates in. The rule
// they enforce: a round refused for any reason other than a restart that was
// actually attempted must leave the attempt count where it was.

const gateNode = "test-node"

// gateProject is a Running, one-service Project assigned to gateNode.
func gateProject() *v1.Project {
	p := &v1.Project{}
	p.Namespace = "default"
	p.Name = "guestbook"
	p.UID = "uid-1"
	p.Status.Phase = v1.ProjectPhaseRunning
	p.Status.NodeRef = gateNode
	p.Status.AssignmentGeneration = 7
	p.Spec.Services = []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}}
	return p
}

// exitedWeb is the fault every gate test starts from: one container that
// exited and can be recovered by a start.
var exitedWeb = []serviceState{{Service: "web", Health: healthStartable, Status: "exited", ExitCode: 1}}

// gateServer serves the re-read and records status writes. current is what
// GET returns; getStatus overrides the response code when non-zero.
type gateServer struct {
	mu        sync.Mutex
	current   *v1.Project
	getStatus int
	gets      int
	updates   []statusUpdate

	// secrets is what GET /api/v1/secrets/{name} serves; secretStatus, when
	// non-zero, overrides it to simulate a server that cannot answer.
	secrets      map[string]v1.Secret
	secretStatus int

	// conditionWrites records every condition PATCH and DELETE, in order.
	// Each one is also applied to current, so the next GET sees it the way a
	// real server's would — which is what the dedupe tests depend on.
	conditionWrites []conditionWrite
}

type conditionWrite struct {
	Op         string // "patch" or "delete"
	Type       v1.ConditionType
	Status     v1.ConditionStatus
	Reason     string
	Message    string
	UID        string
	NodeRef    string
	Generation int64
}

func (gs *gateServer) writes(op string, condType v1.ConditionType) []conditionWrite {
	gs.mu.Lock()
	defer gs.mu.Unlock()
	var out []conditionWrite
	for _, w := range gs.conditionWrites {
		if w.Op == op && w.Type == condType {
			out = append(out, w)
		}
	}
	return out
}

func withoutCondition(conds []v1.Condition, condType v1.ConditionType) []v1.Condition {
	out := make([]v1.Condition, 0, len(conds))
	for _, c := range conds {
		if c.Type != condType {
			out = append(out, c)
		}
	}
	return out
}

func newGateServer(t *testing.T, current *v1.Project) (*gateServer, *Client) {
	t.Helper()
	gs := &gateServer{current: current}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/projects/{name}", func(w http.ResponseWriter, r *http.Request) {
		gs.mu.Lock()
		defer gs.mu.Unlock()
		gs.gets++
		if gs.getStatus != 0 {
			http.Error(w, "injected", gs.getStatus)
			return
		}
		_ = json.NewEncoder(w).Encode(gs.current)
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", func(w http.ResponseWriter, r *http.Request) {
		var req projectStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		gs.mu.Lock()
		gs.updates = append(gs.updates, statusUpdate{
			ProjectName: r.PathValue("name"), Phase: req.Phase, Reason: req.Reason, Message: req.Message,
		})
		gs.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("GET /api/v1/secrets/{name}", func(w http.ResponseWriter, r *http.Request) {
		gs.mu.Lock()
		defer gs.mu.Unlock()
		if gs.secretStatus != 0 {
			http.Error(w, "injected", gs.secretStatus)
			return
		}
		secret, ok := gs.secrets[r.PathValue("name")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(secret)
	})
	mux.HandleFunc("PATCH /api/v1/projects/{name}/conditions/{type}", func(w http.ResponseWriter, r *http.Request) {
		var req conditionPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		condType := v1.ConditionType(r.PathValue("type"))
		gs.mu.Lock()
		defer gs.mu.Unlock()
		cw := conditionWrite{
			Op: "patch", Type: condType, Status: req.Status, Reason: req.Reason, Message: req.Message,
			UID: req.UID, NodeRef: req.NodeRef,
		}
		if req.AssignmentGeneration != nil {
			cw.Generation = *req.AssignmentGeneration
		}
		gs.conditionWrites = append(gs.conditionWrites, cw)
		gs.current.Status.Conditions = append(withoutCondition(gs.current.Status.Conditions, condType),
			v1.Condition{Type: condType, Status: req.Status, Reason: req.Reason, Message: req.Message,
				LastTransitionTime: time.Now()})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /api/v1/projects/{name}/conditions/{type}", func(w http.ResponseWriter, r *http.Request) {
		condType := v1.ConditionType(r.PathValue("type"))
		gs.mu.Lock()
		defer gs.mu.Unlock()
		gs.conditionWrites = append(gs.conditionWrites, conditionWrite{
			Op: "delete", Type: condType,
			UID: r.URL.Query().Get("uid"), NodeRef: r.URL.Query().Get("nodeRef"),
		})
		gs.current.Status.Conditions = withoutCondition(gs.current.Status.Conditions, condType)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return gs, NewClient(zap.NewNop(), srv.URL, gateNode)
}

// gateRuntime counts the two recovery calls, and runs an optional hook inside
// each so a test can observe state at the moment the call happens.
type gateRuntime struct {
	mockRuntime
	preflights   int
	recoveries   int
	preflightErr error
	recoverErr   error
	onPreflight  func()
	onRecover    func()
}

func newGateRuntime() *gateRuntime {
	g := &gateRuntime{}
	g.preflightFn = func(context.Context, *v1.Project, []string) error {
		g.preflights++
		if g.onPreflight != nil {
			g.onPreflight()
		}
		return g.preflightErr
	}
	g.recoverFn = func(context.Context, *v1.Project, []string) error {
		g.recoveries++
		if g.onRecover != nil {
			g.onRecover()
		}
		return g.recoverErr
	}
	return g
}

// volumeGone is what PreflightRecovery returns for a missing Managed volume,
// including the node-local detail that must stay out of the condition.
func volumeGone() error { return volumeGoneNamed("data") }

func volumeGoneNamed(volume string) error {
	return fmt.Errorf("preflight %q: %w", "web", &docker.VolumeUnavailableError{
		Service: "web", Volume: volume, Type: v1.VolumeTypeManaged,
		Detail: "/var/lib/cara/volumes/default/guestbook/" + volume + "/data is missing: stat: no such file or directory",
	})
}

// 1 & 2. Missing data blocks the round before the attempt is counted, and the
// runtime is never asked to recover. No status is written either: Failed is
// terminal for the poll loop, and this Project must be picked up again as
// soon as the data is restored.
func TestRecoveryGateVolumeUnavailableSpendsNoAttempt(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = volumeGone()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Equal(t, 1, rt.preflights)
	assert.Equal(t, 0, tracker.attempts(keyForProject(p)), "blocked is not a failed attempt")
	assert.Equal(t, 0, rt.recoveries, "RecoverServices must not run when the gate refuses")
	assert.Empty(t, gs.updates, "blocked must not be reported as Failed or exhausted")
}

// Blocked is re-evaluated every tick and never converges on exhaustion, no
// matter how long the data stays missing.
func TestRecoveryGateStaysBlockedWithoutExhausting(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = volumeGone()
	tracker, clk := newTestTracker()

	for i := 0; i < 20; i++ {
		recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())
		clk.advance(10 * time.Second)
	}

	assert.Equal(t, 0, tracker.attempts(keyForProject(p)))
	assert.Zero(t, rt.recoveries)
	assert.Empty(t, gs.updates)
}

// 3. The attempt is counted after the gate passes and before the Docker call:
// zero while the preflight runs, one by the time RecoverServices runs.
func TestRecoveryGateCountsTheAttemptOnlyAfterPreflight(t *testing.T) {
	p := gateProject()
	_, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	tracker, _ := newTestTracker()
	key := keyForProject(p)

	var duringPreflight, duringRecover int
	rt.onPreflight = func() { duringPreflight = tracker.attempts(key) }
	rt.onRecover = func() { duringRecover = tracker.attempts(key) }

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Equal(t, 0, duringPreflight)
	assert.Equal(t, 1, duringRecover)
	assert.Equal(t, 1, rt.recoveries)
}

// 4, agent side. The data vanishes between the gate and the Docker call. The
// runtime's own second preflight refuses (proven in the docker package), and
// the attempt stays spent: recovery had formally begun. This is the accepted
// boundary, pinned so nobody "fixes" it by rolling the counter back.
func TestRecoveryGateVolumeLostAfterPreflightKeepsTheAttempt(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.recoverErr = volumeGone()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Equal(t, 1, rt.recoveries)
	assert.Equal(t, 1, tracker.attempts(keyForProject(p)))
	assert.Empty(t, gs.updates)
}

// 5. The OpRecovery claim is held while both preflights and the recovery run,
// and released afterwards. A backup tick in that window loses the TryClaim
// instead of stopping containers underneath the recovery.
func TestRecoveryGateClaimCoversPreflightAndRecovery(t *testing.T) {
	p := gateProject()
	_, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	tracker, _ := newTestTracker()
	coord := backup.NewCoordinator()
	rkey := backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}

	heldBy := func() (backup.Operation, bool) { return coord.Current(rkey) }
	backupWins := func() bool {
		release, ok := coord.TryClaim(rkey, backup.OpBackup)
		if ok {
			release()
		}
		return ok
	}

	var opAtPreflight, opAtRecover backup.Operation
	var backupAtPreflight, backupAtRecover bool
	rt.onPreflight = func() {
		opAtPreflight, _ = heldBy()
		backupAtPreflight = backupWins()
	}
	rt.onRecover = func() {
		opAtRecover, _ = heldBy()
		backupAtRecover = backupWins()
	}

	recoverLocally(t.Context(), client, rt, coord, tracker, p, exitedWeb, zap.NewNop())

	assert.Equal(t, backup.OpRecovery, opAtPreflight)
	assert.Equal(t, backup.OpRecovery, opAtRecover)
	assert.False(t, backupAtPreflight, "a backup must not start during the preflight")
	assert.False(t, backupAtRecover, "a backup must not start during the recovery")
	assert.False(t, coord.IsBusy(rkey), "the claim is released when the round ends")
}

// The claim is released on every exit path, not just the successful one —
// otherwise a single blocked round would lock the Project out of backups.
func TestRecoveryGateReleasesTheClaimWhenRefused(t *testing.T) {
	p := gateProject()
	_, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = volumeGone()
	tracker, _ := newTestTracker()
	coord := backup.NewCoordinator()

	recoverLocally(t.Context(), client, rt, coord, tracker, p, exitedWeb, zap.NewNop())

	assert.False(t, coord.IsBusy(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}))
}

// 6. Every gate ahead of the preflight refuses without spending an attempt,
// and without reaching the runtime at all.
func TestRecoveryGateRefusalsSpendNoAttempt(t *testing.T) {
	cases := []struct {
		name string
		// setup mutates the server's copy (what the re-read returns) and may
		// hold a claim on the coordinator.
		setup func(gs *gateServer, coord *backup.Coordinator, p *v1.Project)
		// wantGet is whether the re-read should have happened at all.
		wantGet bool
	}{
		{
			name: "another operation holds the claim",
			setup: func(_ *gateServer, coord *backup.Coordinator, p *v1.Project) {
				_, ok := coord.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpBackup)
				require.True(t, ok)
			},
			wantGet: false,
		},
		{
			name: "server unreachable",
			setup: func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) {
				gs.getStatus = http.StatusInternalServerError
			},
			wantGet: true,
		},
		{
			name:    "project deleted",
			setup:   func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) { gs.getStatus = http.StatusNotFound },
			wantGet: true,
		},
		{
			name:    "recreated under a new UID",
			setup:   func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) { gs.current.UID = "uid-2" },
			wantGet: true,
		},
		{
			name: "reassigned away and back under a new generation",
			setup: func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) {
				gs.current.Status.AssignmentGeneration = 8
			},
			wantGet: true,
		},
		{
			name:    "assigned to another node",
			setup:   func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) { gs.current.Status.NodeRef = "other-node" },
			wantGet: true,
		},
		{
			name: "moved to Terminating",
			setup: func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) {
				gs.current.Status.Phase = v1.ProjectPhaseTerminating
			},
			wantGet: true,
		},
		{
			name: "entered Maintenance since the poll",
			setup: func(gs *gateServer, _ *backup.Coordinator, _ *v1.Project) {
				gs.current.Status.Conditions = []v1.Condition{{
					Type:               v1.ConditionTypeMaintenance,
					Status:             v1.ConditionTrue,
					LastTransitionTime: time.Now(),
				}}
			},
			wantGet: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := gateProject()
			gs, client := newGateServer(t, gateProject())
			rt := newGateRuntime()
			tracker, _ := newTestTracker()
			coord := backup.NewCoordinator()
			tc.setup(gs, coord, p)

			recoverLocally(t.Context(), client, rt, coord, tracker, p, exitedWeb, zap.NewNop())

			assert.Equal(t, 0, tracker.attempts(keyForProject(p)), "a refused round spends no attempt")
			assert.Zero(t, rt.preflights, "refused before the volume preflight")
			assert.Zero(t, rt.recoveries, "refused before any Docker call")
			assert.Empty(t, gs.updates, "a refused round writes no status")
			if tc.wantGet {
				assert.Equal(t, 1, gs.gets)
			} else {
				assert.Zero(t, gs.gets, "a lost claim returns before asking the server")
			}
		})
	}
}

// Exhaustion is still reached by rounds that genuinely attempted, and is
// reported against the fence of the Project just read.
func TestRecoveryGateReportsExhaustionAfterRealAttempts(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	tracker, clk := newTestTracker()

	run := func() {
		recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())
	}

	for i := 0; i < maxRecoveryAttempts; i++ {
		run()
		if i < len(recoveryBackoff) {
			clk.advance(recoveryBackoff[i])
		}
	}
	require.Equal(t, maxRecoveryAttempts, rt.recoveries)
	require.Empty(t, gs.updates)

	clk.advance(recoveryVerifyTimeout)
	run()

	require.Len(t, gs.updates, 1)
	assert.Equal(t, v1.ProjectPhaseFailed, gs.updates[0].Phase)
	assert.Equal(t, "LocalRestartExhausted", gs.updates[0].Reason)
	assert.Equal(t, maxRecoveryAttempts, rt.recoveries, "exhaustion does not make a fourth attempt")
}

func TestCheckRecoveryFenceAcceptsTheSameAssignment(t *testing.T) {
	assert.NoError(t, checkRecoveryFence(gateProject(), gateProject(), gateNode))
}

// ── RecoveryBlocked ──────────────────────────────────────────────────────────

func blockedCondition(message string) v1.Condition {
	return v1.Condition{
		Type:               v1.ConditionTypeRecoveryBlocked,
		Status:             v1.ConditionTrue,
		Reason:             recoveryBlockedVolumeUnavailable,
		Message:            message,
		LastTransitionTime: time.Now(),
	}
}

// A block is reported to the control plane, fenced by the assignment just
// read, while the phase stays Running so the Project is still polled.
func TestRecoveryBlockedIsReportedAsAFencedCondition(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = volumeGone()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, v1.ConditionTrue, patches[0].Status)
	assert.Equal(t, "VolumeUnavailable", patches[0].Reason)
	assert.Equal(t, `service "web" needs Managed volume "data", which is not available on this node`,
		patches[0].Message)
	assert.NotContains(t, patches[0].Message, "/var/lib", "host paths stay in the agent log")
	assert.NotContains(t, patches[0].Message, "no such file", "OS errors stay in the agent log")
	assert.Equal(t, "uid-1", patches[0].UID)
	assert.Equal(t, gateNode, patches[0].NodeRef)
	assert.Equal(t, int64(7), patches[0].Generation)
	assert.Empty(t, gs.updates, "the phase is not touched")
}

// The same block on every poll is written once. Without the comparison each
// ten-second tick would be a database write and a project.updated event.
func TestRecoveryBlockedIsNotRewrittenWhileUnchanged(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = volumeGone()
	tracker, clk := newTestTracker()

	for i := 0; i < 5; i++ {
		recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())
		clk.advance(10 * time.Second)
	}

	assert.Len(t, gs.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
}

// A different block — another volume, another message — is news and is
// written.
func TestRecoveryBlockedIsRewrittenWhenTheReasonChanges(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	rt.preflightErr = volumeGone()
	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	rt.preflightErr = volumeGoneNamed("other")
	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 2)
	assert.Contains(t, patches[1].Message, "other")
}

// Once the preflight passes, the block is cleared before the attempt is
// spent and before any Docker call.
func TestRecoveryBlockedIsClearedBeforeTheAttempt(t *testing.T) {
	p := gateProject()
	current := gateProject()
	current.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
	gs, client := newGateServer(t, current)
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	var deletesAtRecover int
	rt.onRecover = func() { deletesAtRecover = len(gs.writes("delete", v1.ConditionTypeRecoveryBlocked)) }

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	deletes := gs.writes("delete", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, deletes, 1)
	assert.Equal(t, "uid-1", deletes[0].UID)
	assert.Equal(t, 1, deletesAtRecover, "cleared before RecoverServices ran")
	assert.Equal(t, 1, rt.recoveries)
}

// No block, no clear: an ordinary recovery writes no condition at all.
func TestRecoveryWithoutABlockWritesNoCondition(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Empty(t, gs.conditionWrites)
	assert.Equal(t, 1, rt.recoveries)
}

// Refusals before the preflight say nothing about volumes, so they neither set
// nor clear the condition. A Project that entered Maintenance keeps whatever
// block it had; a claim lost to a backup does not erase it either.
func TestRecoveryRefusalsLeaveTheConditionAlone(t *testing.T) {
	p := gateProject()
	current := gateProject()
	current.Status.Conditions = []v1.Condition{
		blockedCondition("was missing"),
		{Type: v1.ConditionTypeMaintenance, Status: v1.ConditionTrue, LastTransitionTime: time.Now()},
	}
	gs, client := newGateServer(t, current)
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Empty(t, gs.conditionWrites)
}

// A block can end with no recovery at all — the operator restores the data
// and starts the container by hand. The next healthy poll clears it.
func TestHealthyProjectClearsALeftoverBlock(t *testing.T) {
	p := gateProject()
	p.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{{ServiceName: "web", Status: "running"}}, nil
	}
	tracker, _ := newTestTracker()

	healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, p, zap.NewNop())

	assert.Len(t, gs.writes("delete", v1.ConditionTypeRecoveryBlocked), 1)
	assert.Zero(t, rt.preflights, "a healthy Project does not go near recovery")
}

func TestHealthyProjectWithoutABlockWritesNoCondition(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return []docker.ContainerState{{ServiceName: "web", Status: "running"}}, nil
	}
	tracker, _ := newTestTracker()

	healthCheckOne(t.Context(), client, rt, nil, backup.NewCoordinator(), tracker, p, zap.NewNop())

	assert.Empty(t, gs.conditionWrites)
}

// ── RecoveryBlocked by Secrets ───────────────────────────────────────────────

// secretProject is gateProject whose "web" service reads a password from the
// Secret "db-creds".
func secretProject() *v1.Project {
	p := gateProject()
	p.Spec.Services[0].Env = []v1.EnvVar{{
		Name:      "DB_PASSWORD",
		ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Name: "db-creds", Key: "password"}},
	}}
	return p
}

func dbCreds(keys ...string) v1.Secret {
	s := v1.Secret{ObjectMeta: v1.ObjectMeta{Name: "db-creds"}}
	for _, k := range keys {
		s.Spec.Data = append(s.Spec.Data, v1.SecretDataItem{Key: k, Value: "hunter2"})
	}
	return s
}

// A deleted Secret blocks recovery the way a missing volume does: no attempt,
// no Docker call, the phase left alone, and a condition that says why — in
// names only.
func TestRecoveryBlockedBySecretNotFound(t *testing.T) {
	p := secretProject()
	gs, client := newGateServer(t, secretProject())
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "SecretNotFound", patches[0].Reason)
	assert.Equal(t, `service "web" needs Secret "db-creds", which does not exist`, patches[0].Message)
	assert.Equal(t, 0, tracker.attempts(keyForProject(p)))
	assert.Zero(t, rt.recoveries)
	assert.Empty(t, gs.updates)
}

func TestRecoveryBlockedBySecretKeyNotFound(t *testing.T) {
	p := secretProject()
	gs, client := newGateServer(t, secretProject())
	gs.secrets = map[string]v1.Secret{"db-creds": dbCreds("username")}
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "SecretKeyNotFound", patches[0].Reason)
	assert.Equal(t, `service "web" needs key "password" in Secret "db-creds", which does not exist`,
		patches[0].Message)
	assert.NotContains(t, patches[0].Message, "hunter2", "Secret values never reach status")
	assert.Equal(t, 0, tracker.attempts(keyForProject(p)))
	assert.Zero(t, rt.recoveries)
}

// A server that cannot answer is not a block. Nothing is published — a
// transient fault would otherwise flap the condition — and the next poll
// retries without having spent an attempt.
func TestRecoverySecretFetchFailureIsTransient(t *testing.T) {
	p := secretProject()
	gs, client := newGateServer(t, secretProject())
	gs.secretStatus = http.StatusServiceUnavailable
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Empty(t, gs.conditionWrites)
	assert.Equal(t, 0, tracker.attempts(keyForProject(p)))
	assert.Zero(t, rt.recoveries)
}

// Volume back, Secret gone: the block changes reason in one PATCH. There is no
// DELETE in between, so the Project never reads as unblocked.
func TestRecoveryBlockChangesReasonWithoutClearing(t *testing.T) {
	p := secretProject()
	current := secretProject()
	current.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
	gs, client := newGateServer(t, current)
	rt := newGateRuntime() // preflight passes: the volume is back
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Empty(t, gs.writes("delete", v1.ConditionTypeRecoveryBlocked),
		"a later precondition still fails, so the block is never lifted")
	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "SecretNotFound", patches[0].Reason)
}

// Only once the volumes and the Secrets both hold is the block cleared, and
// then the recovery runs with the Secret resolved.
func TestRecoveryBlockClearsOnlyWhenEveryPreconditionHolds(t *testing.T) {
	p := secretProject()
	current := secretProject()
	current.Status.Conditions = []v1.Condition{blockedCondition("was missing")}
	gs, client := newGateServer(t, current)
	gs.secrets = map[string]v1.Secret{"db-creds": dbCreds("password")}
	rt := newGateRuntime()
	var recoveredWith *v1.Project
	rt.recoverFn = func(_ context.Context, project *v1.Project, _ []string) error {
		rt.recoveries++
		recoveredWith = project
		return nil
	}
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	assert.Len(t, gs.writes("delete", v1.ConditionTypeRecoveryBlocked), 1)
	require.Equal(t, 1, rt.recoveries)
	assert.Equal(t, "hunter2", recoveredWith.Spec.Services[0].Env[0].Value,
		"the rebuilt container gets the Secret's value")
}

// A Secret only a still-running service needs is not a precondition for
// recovering a different one: that running container already holds its
// environment.
func TestRecoveryIgnoresSecretsOfServicesItIsNotRecovering(t *testing.T) {
	p := secretProject()
	p.Spec.Services = append(p.Spec.Services, v1.ServiceDef{Name: "worker", Image: "busybox"})
	current := secretProject()
	current.Spec.Services = p.Spec.Services
	gs, client := newGateServer(t, current) // db-creds does not exist
	rt := newGateRuntime()
	tracker, _ := newTestTracker()

	brokenWorker := []serviceState{{Service: "worker", Health: healthStartable, Status: "exited", ExitCode: 1}}
	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, brokenWorker, zap.NewNop())

	assert.Empty(t, gs.conditionWrites, "web's missing Secret does not block worker")
	assert.Equal(t, 1, rt.recoveries)
}

// A container under the service's name that belongs to another assignment
// blocks recovery the way missing data does: no attempt, no Docker call, a
// condition that names the service — and nothing about the labels.
func TestRecoveryBlockedByAStaleContainer(t *testing.T) {
	p := gateProject()
	gs, client := newGateServer(t, gateProject())
	rt := newGateRuntime()
	rt.preflightErr = fmt.Errorf("preflight %q: %w", "web", &docker.ContainerNotOwnedError{
		Container: "guestbook-web", Service: "web", Label: "cara.generation", Want: "7", Got: "6",
	})
	tracker, _ := newTestTracker()

	recoverLocally(t.Context(), client, rt, backup.NewCoordinator(), tracker, p, exitedWeb, zap.NewNop())

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1)
	assert.Equal(t, "StaleContainer", patches[0].Reason)
	assert.Equal(t, `service "web" has a container left by another assignment, which cara will neither start nor remove`,
		patches[0].Message)
	assert.Equal(t, 0, tracker.attempts(keyForProject(p)))
	assert.Zero(t, rt.recoveries)
	assert.Empty(t, gs.updates, "the phase is not touched")
}
