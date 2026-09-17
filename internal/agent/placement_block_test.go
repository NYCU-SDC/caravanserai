package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/restore"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// blockableProject is a Scheduled Project with one Managed volume, placed on a
// Node that has been assigned before — the shape every refusal below starts
// from.
func blockableProject() *v1.Project {
	p := gateProject()
	p.Status.Phase = v1.ProjectPhaseScheduled
	p.Status.AssignmentHistory = v1.AssignmentHistoryKnown
	p.Spec.Volumes = []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}}
	p.Spec.Services[0].VolumeMounts = []v1.VolumeMount{{Name: "data", MountPath: "/data"}}
	return p
}

// A placement refused for want of provable data must not end up Failed.
//
// Failed is terminal for the poll loop — projectsForReconcile drops it — so a
// Project reported Failed here would never be looked at again, not even after
// an operator cleared the stale bytes or fixed the bucket that caused the
// refusal. Every one of these blocks is resolvable, so every one of them has
// to leave the Project reachable.
func TestBlockedPlacementReportsAConditionNotFailure(t *testing.T) {
	p := blockableProject()
	gs, client := newGateServer(t, blockableProject())
	rt := newGateRuntime()
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return nil, nil // no containers yet: this is a placement
	}
	reconciles := 0
	rt.reconcileFn = func(context.Context, *v1.Project) error {
		reconciles++
		return nil
	}

	dataRoot := t.TempDir()
	live := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "data", "data")
	require.NoError(t, os.MkdirAll(live, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(live, "stale.txt"), []byte("not ours"), 0o600))

	coordinator := backup.NewCoordinator()
	backups := &BackupSupport{
		Coordinator: coordinator,
		Restorer:    restore.NewRestorer(missingStore{}, restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop()),
		DataRoot:    dataRoot,
		NodeName:    gateNode,
	}

	logs, logger := recordedLogger()
	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, p, logger)

	// The defect this replaces spent a week invisible because the branch that
	// took it logged at Debug. A refusal nobody can see is the same defect
	// wearing a different name, so the log line is part of the contract.
	warns := logs.FilterLevelExact(zapcore.WarnLevel).All()
	require.NotEmpty(t, warns)
	assert.Contains(t, warns[0].ContextMap(), "reason")

	patches := gs.writes("patch", v1.ConditionTypeRecoveryBlocked)
	require.Len(t, patches, 1, "the refusal has to be visible in Project status")
	assert.Equal(t, string(restore.BlockUnprovenData), patches[0].Reason)
	assert.NotContains(t, patches[0].Message, dataRoot, "status must not publish this node's layout")

	assert.Empty(t, gs.updates, "a refusal is not a phase transition, and never a terminal one")
	assert.Zero(t, reconciles, "nothing may start against data of unknown origin")

	// shouldSupervise requires Running, and the phase was left alone, so
	// backup supervision cannot have begun on this state.
	assert.NotEqual(t, v1.ProjectPhaseRunning, p.Status.Phase)

	got, err := os.ReadFile(filepath.Join(live, "stale.txt"))
	require.NoError(t, err, "refusing is not a licence to delete")
	assert.Equal(t, "not ours", string(got))
}

// A block has to be a pause, not an ending.
//
// Every reason a placement is refused is something a person or a restore can
// resolve — clear the stale bytes, fix the bucket, wait for the backup to
// appear. The Project is therefore left in a phase the poll loop still visits,
// and the next tick has to pick it up and converge without anyone restarting
// the agent. Reporting the first refusal correctly is only half of that claim;
// this is the other half.
func TestBlockedPlacementConvergesOnceTheCauseIsCleared(t *testing.T) {
	p := blockableProject()
	gs, client := newGateServer(t, blockableProject())
	rt := newGateRuntime()
	rt.inspectFn = func(context.Context, *v1.Project) ([]docker.ContainerState, error) {
		return nil, nil
	}
	reconciles := 0
	rt.reconcileFn = func(context.Context, *v1.Project) error {
		reconciles++
		return nil
	}

	dataRoot := t.TempDir()
	stale := filepath.Join(dataRoot, "volumes", p.Namespace, p.Name, "data", "data")
	require.NoError(t, os.MkdirAll(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "stale.txt"), []byte("not ours"), 0o600))

	backups := &BackupSupport{
		Coordinator: backup.NewCoordinator(),
		Restorer: restore.NewRestorer(missingStore{},
			restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop()),
		DataRoot: dataRoot,
		NodeName: gateNode,
	}

	// Tick 1: unproven bytes on disk.
	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, p, zap.NewNop())
	require.Len(t, gs.writes("patch", v1.ConditionTypeRecoveryBlocked), 1)
	require.Zero(t, reconciles)

	// An operator clears what the agent refused to use. Nothing else changes:
	// no restart, no manual status edit, no new assignment.
	require.NoError(t, os.RemoveAll(filepath.Join(dataRoot, "volumes", p.Namespace, p.Name)))

	// Tick 2 starts the way the poll loop does: with a fresh copy from the
	// server. That copy carries the condition tick 1 wrote, which is how the
	// agent knows there is one to clear once the placement succeeds.
	fresh, err := client.GetProject(t.Context(), p.Name)
	require.NoError(t, err)
	_, hasCondition := projectCondition(fresh, v1.ConditionTypeRecoveryBlocked)
	require.True(t, hasCondition, "the refusal must still be visible when the next tick begins")

	// The Project has never been assigned anywhere else, so with the stale
	// bytes gone an empty start is now the correct answer.
	fresh.Status.AssignmentHistory = v1.AssignmentHistoryNeverAssigned
	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, fresh, zap.NewNop())

	assert.Equal(t, 1, reconciles, "the next tick must place the Project without intervention")
	assert.NotEmpty(t, gs.writes("delete", v1.ConditionTypeRecoveryBlocked),
		"the condition must be cleared, or status keeps reporting a refusal that is over")

	require.NotEmpty(t, gs.updates)
	assert.Equal(t, v1.ProjectPhaseRunning, gs.updates[len(gs.updates)-1].Phase)
}
