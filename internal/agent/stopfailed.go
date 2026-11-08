package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// recoveryBlockedStopUnconfirmed is the RecoveryBlocked reason for a Failed
// Project whose containers could not be confirmed stopped, so its stop
// evidence was not reported and its assignment cannot be released.
const recoveryBlockedStopUnconfirmed = "StopUnconfirmed"

// stopOnFailureReasons are the Failed reasons after which this node stops the
// Project and reports it stopped (CARA-92). They are the reasons the server's
// Tier-2 recovery moves a Failed Project to another Node for (CARA-118), and
// a move needs the old workload stopped first. Any other Failed reason — a
// missing Secret, a failed delete — is not moved, so the Project is left as
// it is for an operator.
var stopOnFailureReasons = map[string]bool{
	"LocalRestartExhausted": true,
	"ContainerRestartStuck": true,
	"InspectError":          true,
	"ReconcileError":        true,
	"ContainerExited":       true,
	"ContainerCrashed":      true,
}

// stopFailedClient is the part of the server API stopping a Failed Project
// needs.
type stopFailedClient interface {
	NodeName() string
	GetProject(ctx context.Context, name string) (*v1.Project, error)
	ReportWorkloadStopped(ctx context.Context, projectName string, fence AssignmentFence) error
	PatchProjectCondition(ctx context.Context, projectName string, fence AssignmentFence, condType v1.ConditionType, status v1.ConditionStatus, reason, message string) error
	ClearProjectCondition(ctx context.Context, projectName string, fence AssignmentFence, condType v1.ConditionType) error
}

// stopFailedRuntime is the part of the container runtime it needs.
type stopFailedRuntime interface {
	StopProject(ctx context.Context, project *v1.Project) error
	InspectProject(ctx context.Context, project *v1.Project) ([]docker.ContainerState, error)
}

// workloadLatched reports whether the server holds this node's evidence that
// the Project's current assignment stopped. Such a generation must never be
// started again on this node: its assignment may already have been released,
// and another node may be starting the Project. The evidence is read from the
// Project as the server sent it, so it holds across an Agent restart without
// any state of the Agent's own.
func workloadLatched(p *v1.Project) bool {
	current := v1.AssignmentIdentity{
		ProjectUID: p.UID, NodeName: p.Status.NodeRef, AssignmentGeneration: p.Status.AssignmentGeneration,
	}
	return p.Status.Recovery.WorkloadStoppedFor(current)
}

// failureReason returns the reason the Project was reported Failed with.
func failedReason(p *v1.Project) string {
	for _, c := range p.Status.Conditions {
		if c.Type == v1.ConditionTypePhase {
			return c.Reason
		}
	}
	return ""
}

// stopFailedProjects stops every Failed Project assigned here whose failure a
// different Node may fix, and reports it stopped (CARA-92). Only then can the
// server release the assignment and Tier-2 recovery move the Project.
//
// It runs from the poll loop on the Projects the server just returned, so a
// Failed phase it acts on is already durable on the server. A Project whose
// stop is not confirmed is reported RecoveryBlocked and tried again on the
// next poll; its Failed reason is left as it is, and Tier-1 recovery is not
// entered again.
func stopFailedProjects(ctx context.Context, client stopFailedClient, runtime stopFailedRuntime, busy busyChecker, projects []*v1.Project, logger *zap.Logger) {
	for _, p := range projects {
		if p.Status.Phase != v1.ProjectPhaseFailed || !stopOnFailureReasons[failedReason(p)] || workloadLatched(p) {
			continue
		}
		stopFailedProject(ctx, client, runtime, busy, p, logger.With(zap.String("project", p.Name)))
	}
}

func stopFailedProject(ctx context.Context, client stopFailedClient, runtime stopFailedRuntime, busy busyChecker, p *v1.Project, log *zap.Logger) {
	// Exclusive with a backup, restore or cleanup of the same Project, so
	// nothing starts a container between the stop and the check.
	if busy != nil {
		release, ok := busy.TryClaim(backup.ResourceKey{Namespace: p.Namespace, Name: p.Name}, backup.OpRecovery)
		if !ok {
			log.Debug("Another operation holds the Project; stopping it waits for the next poll")
			return
		}
		defer release()
	}

	// The copy in hand may be seconds old. Stopping a Project that has since
	// been reassigned, applied again or deleted would act on a grant this
	// node no longer holds.
	current, err := client.GetProject(ctx, p.Name)
	if err != nil {
		log.Warn("Could not re-read the Failed Project before stopping it, will retry", zap.Error(err))
		return
	}
	if err := checkStopFence(p, current, client.NodeName()); err != nil {
		log.Info("Failed Project changed before it could be stopped, skipping", zap.Error(err))
		return
	}
	if workloadLatched(current) {
		return
	}

	fence := fenceForProject(current)
	if err := runtime.StopProject(ctx, current); err != nil {
		blockStop(ctx, client, current, fence, fmt.Sprintf("Could not stop the containers: %v", err), log)
		return
	}
	if running, err := stillRunning(ctx, runtime, current); err != nil {
		blockStop(ctx, client, current, fence, fmt.Sprintf("Could not confirm the containers stopped: %v", err), log)
		return
	} else if len(running) > 0 {
		blockStop(ctx, client, current, fence,
			"Containers still running after the stop: "+strings.Join(running, ", "), log)
		return
	}

	if err := client.ReportWorkloadStopped(ctx, current.Name, fence); err != nil {
		if errors.Is(err, ErrStaleAssignment) || errors.Is(err, ErrProjectNotFound) {
			log.Info("Failed Project no longer holds this assignment; nothing to report", zap.Error(err))
			return
		}
		// Stopped but not recorded: the next poll stops (a no-op) and
		// reports again.
		log.Warn("Failed Project stopped but the evidence was not accepted, will retry", zap.Error(err))
		return
	}
	log.Info("Failed Project stopped; reported so it can be moved to another node",
		zap.String("reason", failedReason(current)))
	clearRecoveryBlocked(ctx, client, current, log)
}

// stillRunning returns the services of this assignment whose container is
// not stopped. A container of another assignment under the same name is not
// this one's, and not counted.
func stillRunning(ctx context.Context, runtime stopFailedRuntime, p *v1.Project) ([]string, error) {
	states, err := runtime.InspectProject(ctx, p)
	if err != nil {
		return nil, err
	}
	var running []string
	for _, s := range states {
		if s.NotOwned != nil {
			continue
		}
		switch s.Status {
		case "exited", "created", "dead":
		default:
			// running, restarting, paused or anything unrecognised: a process
			// that may still run.
			running = append(running, s.ServiceName+"("+s.Status+")")
		}
	}
	return running, nil
}

// blockStop reports that a Failed Project could not be confirmed stopped. The
// Failed reason is a different condition (Phase) and is left as it is.
func blockStop(ctx context.Context, client stopFailedClient, p *v1.Project, fence AssignmentFence, message string, log *zap.Logger) {
	log.Warn("Failed Project could not be confirmed stopped; its assignment stays here, will retry",
		zap.String("reason", recoveryBlockedStopUnconfirmed), zap.String("detail", message))
	reportRecoveryBlocked(ctx, client, p, recoveryBlockedStopUnconfirmed, message, log)
}

// checkStopFence is checkRecoveryFence for a Failed Project.
func checkStopFence(observed, current *v1.Project, nodeName string) error {
	switch {
	case current.UID != observed.UID:
		return fmt.Errorf("project UID changed from %q to %q", observed.UID, current.UID)
	case current.Status.AssignmentGeneration != observed.Status.AssignmentGeneration:
		return fmt.Errorf("assignment generation changed from %d to %d",
			observed.Status.AssignmentGeneration, current.Status.AssignmentGeneration)
	case current.Status.NodeRef != nodeName:
		return fmt.Errorf("project is assigned to node %q, not this node %q", current.Status.NodeRef, nodeName)
	case current.Status.Phase != v1.ProjectPhaseFailed:
		return fmt.Errorf("project phase is %s, not Failed", current.Status.Phase)
	case !stopOnFailureReasons[failedReason(current)]:
		return fmt.Errorf("failed for %q, which is not moved to another node", failedReason(current))
	}
	return nil
}
