package testhelper

import (
	"context"
	"testing"
	"time"

	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/stretchr/testify/require"
)

// ReleaseCleanly takes the named Project off its Node the way a fully
// cooperative Agent would: the Agent confirms the workload stopped, the
// Rescheduler releases the assignment on that evidence, and the Agent then
// reports its cleanup obligation resolved. The Project ends Pending and
// schedulable again.
//
// For tests whose subject is something after the release — generation
// counting, latch clearing, condition timestamps — not the release gate
// itself.
func ReleaseCleanly(t *testing.T, s *pgstore.Store, name string) {
	t.Helper()
	ctx := context.Background()

	p, err := s.GetProject(ctx, name)
	require.NoError(t, err)
	ref := store.AssignmentRef{UID: p.ObjectMeta.UID, NodeRef: p.Status.NodeRef, Generation: p.Status.AssignmentGeneration}

	require.NoError(t, s.RecordWorkloadStopped(ctx, name, ref, time.Now()))
	released, err := adapter.NewProjectStoreAdapter(s).ReleaseAssignment(ctx, &controller.ProjectSnapshot{
		Name:                 name,
		UID:                  ref.UID,
		Phase:                p.Status.Phase,
		NodeRef:              ref.NodeRef,
		AssignmentGeneration: ref.Generation,
	})
	require.NoError(t, err)
	require.True(t, released, "release with matching WorkloadStopped evidence was refused")
	require.NoError(t, s.RecordResourcesRemoved(ctx, name, ref, time.Now()))
}
