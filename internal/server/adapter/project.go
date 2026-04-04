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

func (a *ProjectStoreAdapter) SetProjectScheduled(ctx context.Context, name, nodeRef string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhaseScheduled
	project.Status.NodeRef = nodeRef
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

func (a *ProjectStoreAdapter) SetProjectPhase(ctx context.Context, name string, phase v1.ProjectPhase, reason, message string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = phase
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               v1.ConditionTypePhase,
		Status:             v1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == v1.ConditionTypePhase {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
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
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhasePending
	project.Status.NodeRef = ""
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               v1.ConditionTypePhase,
		Status:             v1.ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady; project reset to Pending for rescheduling",
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == v1.ConditionTypePhase {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// SetTerminatingAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the TerminatingAt condition to record the time at which
// the rescheduler first observed this project as stranded on a NotReady node.
func (a *ProjectStoreAdapter) SetTerminatingAt(ctx context.Context, name string, at time.Time) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	cond := v1.Condition{
		Type:               v1.ConditionTypeTerminatingAt,
		Status:             v1.ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady while project was Terminating; force-termination timeout clock started",
		LastTransitionTime: at,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == v1.ConditionTypeTerminatingAt {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// SetNotReadyAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the NotReadyAt condition to record the time at which
// the rescheduler first observed this Running project as stranded on a NotReady
// node.  The grace period clock starts from this timestamp.
func (a *ProjectStoreAdapter) SetNotReadyAt(ctx context.Context, name string, at time.Time) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	cond := v1.Condition{
		Type:               v1.ConditionTypeNotReadyAt,
		Status:             v1.ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady while project was Running; running grace period clock started",
		LastTransitionTime: at,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == v1.ConditionTypeNotReadyAt {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// ForceTerminated satisfies controller.ReschedulerProjectStore.
// Transitions the project to Terminated and records a Phase condition with
// reason=TerminationTimeout.
func (a *ProjectStoreAdapter) ForceTerminated(ctx context.Context, name string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhaseTerminated
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               v1.ConditionTypePhase,
		Status:             v1.ConditionTrue,
		Reason:             "TerminationTimeout",
		Message:            "Node was NotReady for too long; project force-terminated. Docker resources on the node may need manual cleanup.",
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == v1.ConditionTypePhase {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}
