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
)

// A placement refused for want of provable data must not end up Failed.
//
// Failed is terminal for the poll loop — projectsForReconcile drops it — so a
// Project reported Failed here would never be looked at again, not even after
// an operator cleared the stale bytes or fixed the bucket that caused the
// refusal. Every one of these blocks is resolvable, so every one of them has
// to leave the Project reachable.
func TestBlockedPlacementReportsAConditionNotFailure(t *testing.T) {
	scheduled := func() *v1.Project {
		p := gateProject()
		p.Status.Phase = v1.ProjectPhaseScheduled
		p.Status.AssignmentHistory = v1.AssignmentHistoryKnown
		p.Spec.Volumes = []v1.VolumeDef{{Name: "data", Type: v1.VolumeTypeManaged}}
		p.Spec.Services[0].VolumeMounts = []v1.VolumeMount{{Name: "data", MountPath: "/data"}}
		return p
	}

	p := scheduled()
	gs, client := newGateServer(t, scheduled())
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
		Coordinator:      coordinator,
		Restorer:         restore.NewRestorer(missingStore{}, restore.Config{DataRoot: dataRoot, NodeName: gateNode}, zap.NewNop()),
		DataRoot:         dataRoot,
		NodeName:         gateNode,
		StrictProvenance: true,
	}

	reconcileOne(t.Context(), client, rt, &recordingRoutes{}, backups, p, zap.NewNop())

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
