//go:build e2e

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/store"
	"NYCU-SDC/caravanserai/test/integration/controllerhelper"

	"github.com/stretchr/testify/require"
)

// TestTerminationLifecycle validates:
//
//  1. Create a Node + Project, wait for Scheduled
//  2. Update project phase to Running (simulate Agent report)
//  3. Update project phase to Terminating (simulate delete request)
//  4. Update project phase to Terminated (simulate Agent cleanup)
//  5. Wait for TerminationController to delete the record from the store
func TestTerminationLifecycle(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	s.Start(t)

	ctx := context.Background()

	// Create a Ready node.
	node := &v1.Node{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"},
		ObjectMeta: v1.ObjectMeta{Name: "term-node-01"},
		Spec:       v1.NodeSpec{Hostname: "term-node-01"},
		Status: v1.NodeStatus{
			State:         v1.NodeStateReady,
			Allocatable:   controllerhelper.RoomyAllocatable(),
			LastHeartbeat: time.Now().UTC(),
		},
	}
	require.NoError(t, s.Store.CreateNode(ctx, node))

	// Create a Pending project.
	project := &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: "term-project-01"},
		Status: v1.ProjectStatus{
			Phase: v1.ProjectPhasePending,
		},
	}
	require.NoError(t, s.Store.CreateProject(ctx, project))

	// Wait for scheduling.
	controllerhelper.WaitForProjectPhase(t, s.Store, 15*time.Second, "term-project-01", v1.ProjectPhaseScheduled)

	// Simulate Agent reporting Running.
	p, err := s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseRunning
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Simulate delete request: set to Terminating.
	p, err = s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseTerminating
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Simulate Agent cleanup: ResourcesRemoved for its assignment, then
	// Terminated. Without the evidence the record would be kept.
	p, err = s.Store.GetProject(ctx, "term-project-01")
	require.NoError(t, err)
	p.Status.Phase = v1.ProjectPhaseTerminated
	p.Status.EnsureRecovery().CleanupObligations = []v1.CleanupObligation{
		removedObligation(p.ObjectMeta.UID, p.Status.NodeRef, p.Status.AssignmentGeneration),
	}
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, "term-project-01", p.Status))

	// Wait for TerminationController to delete the project from the store.
	controllerhelper.WaitForProjectNotFound(t, s.Store, 15*time.Second, "term-project-01")
}

func removedObligation(uid, node string, generation int64) v1.CleanupObligation {
	now := time.Now().UTC()
	return v1.CleanupObligation{
		Assignment:           v1.AssignmentIdentity{ProjectUID: uid, NodeName: node, AssignmentGeneration: generation},
		ReleasedAt:           now,
		ReleaseAuthorization: v1.ReleaseByDeletion,
		ResourcesRemoved:     &v1.ResourcesRemovedEvidence{ObservedAt: now},
	}
}

// eventsOnly is a resync interval no test waits for: with it, the only thing
// that can bring a Project back to a controller is an event.
const eventsOnly = time.Hour

// settle gives the reconciles already queued by earlier writes time to run, so
// that what happens next can only be caused by the write that follows.
func settle() { time.Sleep(time.Second) }

// A Terminated Project an earlier Node still owes cleanup for keeps its record,
// says what it waits for, and is finalized once that Node reports
// ResourcesRemoved. The periodic resync is pushed out of reach, so it is the
// report's event alone that brings it back to the controller.
func TestTerminationWaitsForEveryObligation(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger,
		controller.WithSeedInterval(eventsOnly))
	s.TruncateAll(t)
	s.Start(t)

	ctx := context.Background()
	const name = "term-owed"
	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Status:     v1.InitialProjectStatus(),
	}))
	p, err := s.Store.GetProject(ctx, name)
	require.NoError(t, err)
	uid := p.ObjectMeta.UID

	// Ran on node-old at generation 1, then on node-new at generation 2, and
	// was deleted there. node-new has cleaned up; node-old has not.
	p.Status.Phase = v1.ProjectPhaseTerminated
	p.Status.NodeRef = "node-new"
	p.Status.AssignmentGeneration = 2
	p.Status.AssignmentHistory = v1.AssignmentHistoryKnown
	owedByOld := removedObligation(uid, "node-old", 1)
	owedByOld.ResourcesRemoved = nil
	p.Status.EnsureRecovery().CleanupObligations = []v1.CleanupObligation{owedByOld, removedObligation(uid, "node-new", 2)}
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, name, p.Status))

	controllerhelper.WaitForCondition(t, 15*time.Second, 100*time.Millisecond, "FinalizationBlocked naming node-old", func() bool {
		got, err := s.Store.GetProject(ctx, name)
		if err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == v1.ConditionTypeFinalizationBlocked {
				return strings.Contains(c.Message, "node node-old generation 1")
			}
		}
		return false
	})
	settle()
	_, err = s.Store.GetProject(ctx, name)
	require.NoError(t, err, "the record was deleted while node-old still owed its cleanup")

	require.NoError(t, s.Store.RecordResourcesRemoved(ctx, name,
		store.AssignmentRef{UID: uid, NodeRef: "node-old", Generation: 1}, time.Now()))

	controllerhelper.WaitForProjectNotFound(t, s.Store, 15*time.Second, name)
}

// blockedFor waits until name carries FinalizationBlocked mentioning want.
func blockedFor(t *testing.T, s *controllerhelper.Suite, name, want string) {
	t.Helper()
	controllerhelper.WaitForCondition(t, 15*time.Second, 100*time.Millisecond, "FinalizationBlocked: "+want, func() bool {
		got, err := s.Store.GetProject(context.Background(), name)
		if err != nil {
			return false
		}
		for _, c := range got.Status.Conditions {
			if c.Type == v1.ConditionTypeFinalizationBlocked {
				return strings.Contains(c.Message, want)
			}
		}
		return false
	})
}

// unassignedTerminating writes name as the delete API leaves a previously
// assigned Project deleted while Pending: Terminating, no current assignment,
// and the obligations its earlier assignments left.
func unassignedTerminating(t *testing.T, s *controllerhelper.Suite, name string, history v1.AssignmentHistory, obligations ...v1.CleanupObligation) string {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.Store.CreateProject(ctx, &v1.Project{
		TypeMeta:   v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"},
		ObjectMeta: v1.ObjectMeta{Name: name},
		Status:     v1.InitialProjectStatus(),
	}))
	p, err := s.Store.GetProject(ctx, name)
	require.NoError(t, err)
	for i := range obligations {
		obligations[i].Assignment.ProjectUID = p.ObjectMeta.UID
	}
	p.Status.Phase = v1.ProjectPhaseTerminating
	p.Status.NodeRef = ""
	p.Status.AssignmentGeneration = 2
	p.Status.AssignmentHistory = history
	p.Status.EnsureRecovery().CleanupObligations = obligations
	require.NoError(t, s.Store.UpdateProjectStatus(ctx, name, p.Status))
	return p.ObjectMeta.UID
}

// A previously assigned Project deleted while Pending has no current
// assignment, so no Agent will report it Terminated. The server finalizes it
// once the Nodes that ran it earlier have reported their cleanup.
func TestTerminationFinalizesADeletionWithNoCurrentAssignment(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger,
		controller.WithSeedInterval(eventsOnly))
	s.TruncateAll(t)
	s.Start(t)

	const name = "term-unassigned"
	owed := removedObligation("", "node-old", 2)
	owed.ResourcesRemoved = nil
	uid := unassignedTerminating(t, s, name, v1.AssignmentHistoryKnown, owed, removedObligation("", "node-older", 1))

	blockedFor(t, s, name, "node node-old generation 2")
	settle()
	got, err := s.Store.GetProject(context.Background(), name)
	require.NoError(t, err, "the record was deleted while node-old still owed its cleanup")
	require.Equal(t, v1.ProjectPhaseTerminating, got.Status.Phase)

	require.NoError(t, s.Store.RecordResourcesRemoved(context.Background(), name,
		store.AssignmentRef{UID: uid, NodeRef: "node-old", Generation: 2}, time.Now()))

	controllerhelper.WaitForProjectNotFound(t, s.Store, 15*time.Second, name)
}

// Without a known history nothing proves which Nodes may still hold the
// Project, so even an empty obligation list keeps the record.
func TestTerminationKeepsADeletionWithUnknownHistory(t *testing.T) {
	s := controllerhelper.NewSuite(t, shared.pool, shared.databaseURL, shared.logger)
	s.TruncateAll(t)
	s.Start(t)

	const name = "term-unknown"
	unassignedTerminating(t, s, name, v1.AssignmentHistoryUnknown)

	blockedFor(t, s, name, "assignment history is Unknown")
	time.Sleep(3 * time.Second) // several resyncs
	_, err := s.Store.GetProject(context.Background(), name)
	require.NoError(t, err, "a Project of unknown history was finalized")
}
