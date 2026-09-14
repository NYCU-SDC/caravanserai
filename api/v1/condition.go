// Package v1 defines the shared API types for Caravanserai resources.
//
// Condition usage across resource kinds:
//
//	Node (node_types.go)
//	  ConditionTypeReady — set by NodeHealthController whenever the node's
//	  heartbeat state changes.  Status=True means the agent is healthy;
//	  Status=False means the heartbeat timed out.
//
//	  ConditionTypeDiskPressure — set by NodeConditionController based on
//	  the Agent's reported Capacity and Allocatable values.  Status=True
//	  means the node's allocatable disk is below 15% of capacity.
//
//	  ConditionTypeMemoryPressure — set by NodeConditionController based on
//	  the Agent's reported Capacity and Allocatable values.  Status=True
//	  means the node's allocatable memory is below 10% of capacity.
//
//	Project (project_types.go)
//	  ConditionTypePhase — updated on every lifecycle phase transition to carry
//	  the machine-readable Reason and human-readable Message.  Status is always
//	  True; the field acts as a structured changelog, not a health signal.
//
//	  ConditionTypeTerminatingAt — written once by ProjectReschedulerController
//	  when it first observes a Terminating project on a NotReady node.
//	  LastTransitionTime is the start of the force-termination timeout clock.
//	  The condition is never updated after it is set; only read.
//
//	  ConditionTypeNotReadyAt — written by ProjectReschedulerController when it
//	  observes a Running project on a NotReady node without a clock for the
//	  current failure. LastTransitionTime is the start of the running
//	  grace-period clock.
//
//	  It measures one incident, not the Project's lifetime. A node can go
//	  NotReady, recover, and fail again; the second failure gets its own full
//	  grace period, decided by comparing the clock against the node's last
//	  heartbeat rather than by trusting that something removed the old one.
//	  Anything reading this condition must make the same comparison — a
//	  timestamp alone cannot say which failure it belongs to.
//
//	  ConditionTypeMaintenance — set and cleared by the agent around an
//	  operation that stops containers on purpose, such as a backup.
//
//	  ConditionTypeRecoveryBlocked — set by the agent when it has refused to
//	  recover a Project's containers because doing so would be unsafe, and
//	  cleared once the reason is gone.
package v1

import "time"

// ConditionStatus mirrors the Kubernetes convention: "True", "False", or "Unknown".
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// ConditionType is a machine-readable identifier for a Condition.
// Using a named type instead of bare string provides compile-time safety and
// prevents typos when referencing well-known condition types.
type ConditionType string

const (
	// ConditionTypeReady indicates whether the resource is fully operational.
	ConditionTypeReady ConditionType = "Ready"

	// ConditionTypePhase carries the reason and human-readable message for the
	// most recent lifecycle phase transition.  It acts as a structured changelog
	// entry: whenever a resource moves to a new phase the writer updates this
	// condition with a CamelCase Reason (e.g. "NodeNotReady", "AgentReady") and
	// a Message explaining why.  Status is always True — the condition records
	// what happened, not whether something is healthy.
	ConditionTypePhase ConditionType = "Phase"

	// ConditionTypeTerminatingAt records the timestamp at which the rescheduler
	// first observed a Terminating project on a NotReady node.
	ConditionTypeTerminatingAt ConditionType = "TerminatingAt"

	// ConditionTypeNotReadyAt records the timestamp at which the rescheduler
	// first observed a Running project on a NotReady node.
	ConditionTypeNotReadyAt ConditionType = "NotReadyAt"

	// ConditionTypeMaintenance indicates that the agent has deliberately
	// stopped a Project's containers for an operation such as a Managed
	// volume backup.  Status=True with Reason="BackingUp" means the Project
	// is not broken even though its containers are down.
	//
	// It is observability, not a lock: the agent's own poll loop is held off
	// by in-process state, because a condition written over the network can
	// fail or arrive late.  Consumers should treat the condition as expired
	// after MaintenanceStaleAfter so a crashed operation cannot mark a
	// Project as under maintenance forever.
	ConditionTypeMaintenance ConditionType = "Maintenance"

	// ConditionTypeRecoveryBlocked indicates that the agent found a Project's
	// containers down and deliberately did not restart them, because a
	// precondition for doing so safely does not hold. Status=True with
	// Reason="VolumeUnavailable" means a volume a service mounts is missing
	// from the node, and starting the container would have run it against
	// empty data.
	//
	// The phase stays Running on purpose. Failed is terminal for the agent's
	// poll loop, so a Project reported Failed would never be retried even
	// after the data is restored; Running keeps it under observation, and the
	// agent clears this condition and resumes recovery on its own once the
	// precondition holds again. Until then the Project is down and nothing in
	// cara will bring it back — this condition is how that becomes visible
	// without logging into the node.
	ConditionTypeRecoveryBlocked ConditionType = "RecoveryBlocked"

	// ConditionTypeDiskPressure indicates whether the node's disk usage is
	// approaching capacity. Set by the NodeConditionController based on
	// the Agent's reported Capacity and Allocatable values.
	ConditionTypeDiskPressure ConditionType = "DiskPressure"

	// ConditionTypeMemoryPressure indicates whether the node's memory usage
	// is approaching capacity. Set by the NodeConditionController based on
	// the Agent's reported Capacity and Allocatable values.
	ConditionTypeMemoryPressure ConditionType = "MemoryPressure"
)

// MaintenanceStaleAfter is how long a Maintenance condition stays meaningful.
// An agent that dies mid-backup leaves the condition behind with nothing to
// clear it, so readers must ignore one older than this rather than treat the
// Project as permanently under maintenance.
const MaintenanceStaleAfter = 30 * time.Minute

// IsMaintenanceActive reports whether conditions carry a Maintenance condition
// that is both True and fresh, as of now.
func IsMaintenanceActive(conditions []Condition, now time.Time) bool {
	for _, c := range conditions {
		if c.Type != ConditionTypeMaintenance {
			continue
		}
		if c.Status != ConditionTrue {
			return false
		}
		return now.Sub(c.LastTransitionTime) < MaintenanceStaleAfter
	}
	return false
}

// UpsertCondition replaces the condition carrying cond.Type in conditions, or
// appends it, returning the result.
//
// LastTransitionTime and LastHeartbeatTime are carried over from the stored
// condition when the incoming one says the same thing — same Status, Reason and
// Message — and transitioned is false.
//
// transitioned is how a caller reports a change the condition cannot see. The
// Phase condition is the case that needs it: its Status is always True and the
// value that actually moved lives in ProjectStatus.Phase, outside the condition
// entirely. A Project can also carry a stale Phase condition, because
// SetProjectScheduled moves the phase without touching conditions — so a node
// flapping during scheduling can produce Scheduled -> Pending with an unchanged
// Reason, which without this flag would keep a timestamp from the previous
// visit to Pending.
//
// Callers re-asserting a condition they report continuously pass false. The
// field means "when the Status last changed"; refreshing it on an unchanged
// re-assertion misreports that, and it also defeats every downstream no-op
// guard, because a status whose bytes differ is a status that must be written.
// The agent re-reports the same phase, reason and message on every poll tick
// for every Project on every node, so a moving timestamp turns each of those
// into a database write and a project.updated event carrying no news.
//
// The rescheduler's TerminatingAt and NotReadyAt clocks are the opposite case
// and pass true. Their whole content is the timestamp: Status, Reason and
// Message are fixed strings, identical on the first incident and the tenth, so
// the equality check above cannot tell a restart from a re-assertion and would
// silently keep the previous incident's start time. They are written only when
// a clock should begin, never on a tick, so there is no no-op traffic to
// protect against — and a clock that cannot be restarted is not a clock.
func UpsertCondition(conditions []Condition, cond Condition, transitioned bool) []Condition {
	for i := range conditions {
		if conditions[i].Type != cond.Type {
			continue
		}
		if !transitioned &&
			conditions[i].Status == cond.Status &&
			conditions[i].Reason == cond.Reason &&
			conditions[i].Message == cond.Message {
			cond.LastTransitionTime = conditions[i].LastTransitionTime
			cond.LastHeartbeatTime = conditions[i].LastHeartbeatTime
		}
		conditions[i] = cond
		return conditions
	}
	return append(conditions, cond)
}

// Condition describes a single observable aspect of a resource's state.
// It mirrors the Kubernetes Condition pattern so the mental model stays familiar.
type Condition struct {
	// Type is a machine-readable identifier, e.g. "Ready", "Phase".
	Type ConditionType `json:"type" yaml:"type"`

	// Status is one of True, False, Unknown.
	Status ConditionStatus `json:"status" yaml:"status"`

	// LastHeartbeatTime is when this condition was last sampled.
	LastHeartbeatTime time.Time `json:"lastHeartbeatTime,omitempty" yaml:"lastHeartbeatTime,omitempty"`

	// LastTransitionTime is when the Status last changed.
	LastTransitionTime time.Time `json:"lastTransitionTime,omitempty" yaml:"lastTransitionTime,omitempty"`

	// Reason is a CamelCase word summarising why the condition has this status.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`

	// Message is a human-readable explanation.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
}
