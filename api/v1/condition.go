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
//	  ConditionTypeNotReadyAt — written once by ProjectReschedulerController
//	  when it first observes a Running project on a NotReady node.
//	  LastTransitionTime is the start of the running grace-period clock.
//	  The condition is never updated after it is set; only read.
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

	// ConditionTypeDiskPressure indicates whether the node's disk usage is
	// approaching capacity. Set by the NodeConditionController based on
	// the Agent's reported Capacity and Allocatable values.
	ConditionTypeDiskPressure ConditionType = "DiskPressure"

	// ConditionTypeMemoryPressure indicates whether the node's memory usage
	// is approaching capacity. Set by the NodeConditionController based on
	// the Agent's reported Capacity and Allocatable values.
	ConditionTypeMemoryPressure ConditionType = "MemoryPressure"
)

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
