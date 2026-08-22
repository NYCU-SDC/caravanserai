// Package adapter bridges the broad postgres.Store CRUD interface and the
// narrow controller-specific store interfaces.  Keeping the adapters here
// (rather than in cmd/cara-server/main.go) makes them independently readable
// and avoids growing main.go with every new controller method.
package adapter

import (
	"context"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
)

// Compile-time interface satisfaction checks.
var (
	_ controller.SchedulerProjectStore   = (*ProjectStoreAdapter)(nil)
	_ controller.TerminationProjectStore = (*ProjectStoreAdapter)(nil)
	_ controller.ReschedulerProjectStore = (*ProjectStoreAdapter)(nil)
)

// ProjectStoreAdapter wraps *pgstore.Store and satisfies
// controller.SchedulerProjectStore, controller.TerminationProjectStore, and
// controller.ReschedulerProjectStore.
type ProjectStoreAdapter struct {
	s *pgstore.Store
}

// NewProjectStoreAdapter returns a ProjectStoreAdapter backed by s.
func NewProjectStoreAdapter(s *pgstore.Store) *ProjectStoreAdapter {
	return &ProjectStoreAdapter{s: s}
}

func (a *ProjectStoreAdapter) ListProjectNamesByPhase(ctx context.Context, phase v1.ProjectPhase) ([]string, error) {
	projects, err := a.s.ListProjectsByPhase(ctx, phase)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names, nil
}

func (a *ProjectStoreAdapter) GetProjectPhase(ctx context.Context, name string) (v1.ProjectPhase, string, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return "", "", err
	}
	return project.Status.Phase, project.Status.NodeRef, nil
}

// upsertCondition replaces the condition carrying cond.Type, or appends it.
//
// Conditions are keyed by Type, so this merge is safe to re-run: applying it to
// a status that already carries the condition produces the same result as
// applying it to one that does not. That matters because every caller below
// hands it to UpdateProjectStatusWithRetry, which may run the closure again
// against freshly read state.
func upsertCondition(status *v1.ProjectStatus, cond v1.Condition) {
	for i := range status.Conditions {
		if status.Conditions[i].Type == cond.Type {
			status.Conditions[i] = cond
			return
		}
	}
	status.Conditions = append(status.Conditions, cond)
}

func (a *ProjectStoreAdapter) SetProjectScheduled(ctx context.Context, name, nodeRef string) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Phase = v1.ProjectPhaseScheduled
		status.NodeRef = nodeRef
		return nil
	})
}

func (a *ProjectStoreAdapter) SetProjectPhase(ctx context.Context, name string, phase v1.ProjectPhase, reason, message string) error {
	// Fixed before the closure, not inside it: the closure may run more than
	// once, and this records when the transition was observed rather than when
	// a retry happened to land.
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Phase = phase
		upsertCondition(status, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		})
		return nil
	})
}

func (a *ProjectStoreAdapter) DeleteProject(ctx context.Context, name string) error {
	return a.s.DeleteProject(ctx, name)
}

// ListProjectsByNodeRef satisfies controller.ReschedulerProjectStore.
// It converts api/v1 Projects into controller.ProjectSnapshot values.
func (a *ProjectStoreAdapter) ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*controller.ProjectSnapshot, error) {
	projects, err := a.s.ListProjectsByNodeRef(ctx, nodeRef, phases)
	if err != nil {
		return nil, err
	}
	snapshots := make([]*controller.ProjectSnapshot, len(projects))
	for i, p := range projects {
		conditions := make([]controller.ConditionSnapshot, len(p.Status.Conditions))
		for j, c := range p.Status.Conditions {
			conditions[j] = controller.ConditionSnapshot{
				Type:               c.Type,
				LastTransitionTime: c.LastTransitionTime,
			}
		}
		snapshots[i] = &controller.ProjectSnapshot{
			Name:       p.Name,
			Phase:      p.Status.Phase,
			NodeRef:    p.Status.NodeRef,
			Conditions: conditions,
		}
	}
	return snapshots, nil
}

// SetProjectPending satisfies controller.ReschedulerProjectStore.
// Clears nodeRef, sets phase=Pending, and records a Phase condition with
// reason=NodeNotReady.
func (a *ProjectStoreAdapter) SetProjectPending(ctx context.Context, name string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Phase = v1.ProjectPhasePending
		status.NodeRef = ""
		upsertCondition(status, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady; project reset to Pending for rescheduling",
			LastTransitionTime: now,
		})
		return nil
	})
}

// SetTerminatingAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the TerminatingAt condition to record the time at which
// the rescheduler first observed this project as stranded on a NotReady node.
func (a *ProjectStoreAdapter) SetTerminatingAt(ctx context.Context, name string, at time.Time) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		upsertCondition(status, v1.Condition{
			Type:               v1.ConditionTypeTerminatingAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady while project was Terminating; force-termination timeout clock started",
			LastTransitionTime: at,
		})
		return nil
	})
}

// SetNotReadyAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the NotReadyAt condition to record the time at which
// the rescheduler first observed this Running project as stranded on a NotReady
// node.  The grace period clock starts from this timestamp.
func (a *ProjectStoreAdapter) SetNotReadyAt(ctx context.Context, name string, at time.Time) error {
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		upsertCondition(status, v1.Condition{
			Type:               v1.ConditionTypeNotReadyAt,
			Status:             v1.ConditionTrue,
			Reason:             "NodeNotReady",
			Message:            "Node went NotReady while project was Running; running grace period clock started",
			LastTransitionTime: at,
		})
		return nil
	})
}

// ForceTerminated satisfies controller.ReschedulerProjectStore.
// Transitions the project to Terminated and records a Phase condition with
// reason=TerminationTimeout.
func (a *ProjectStoreAdapter) ForceTerminated(ctx context.Context, name string) error {
	now := time.Now().UTC()
	return a.s.UpdateProjectStatusWithRetry(ctx, name, func(status *v1.ProjectStatus) error {
		status.Phase = v1.ProjectPhaseTerminated
		upsertCondition(status, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             "TerminationTimeout",
			Message:            "Node was NotReady for too long; project force-terminated. Docker resources on the node may need manual cleanup.",
			LastTransitionTime: now,
		})
		return nil
	})
}
