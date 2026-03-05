package v1

import "time"

// APIVersion is the versioned identifier used in YAML manifests.
const APIVersion = "caravanserai/v1"

// TypeMeta holds the Kind and APIVersion fields present in every resource.
type TypeMeta struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind"       yaml:"kind"`
}

// ObjectMeta holds identity and classification metadata common to all resources.
type ObjectMeta struct {
	// Name is the unique identifier within its Kind namespace.
	Name string `json:"name" yaml:"name"`

	// Labels are arbitrary key/value pairs used for selection and grouping.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Annotations are non-identifying metadata (e.g. human-readable hints).
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`

	// CreatedAt is set by the server on first write.
	CreatedAt time.Time `json:"createdAt,omitempty" yaml:"createdAt,omitempty"`

	// UpdatedAt is set by the server on every write.
	UpdatedAt time.Time `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
}

// ConditionStatus mirrors the Kubernetes convention: "True", "False", or "Unknown".
type ConditionStatus string

const (
	ConditionTrue    ConditionStatus = "True"
	ConditionFalse   ConditionStatus = "False"
	ConditionUnknown ConditionStatus = "Unknown"
)

// Condition describes a single observable aspect of a resource's state.
// It mirrors the Kubernetes Condition pattern so the mental model stays familiar.
type Condition struct {
	// Type is a machine-readable identifier, e.g. "Ready", "DiskPressure".
	Type string `json:"type" yaml:"type"`

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

// ResourceList is a named set of resource quantities, e.g. cpu, memory, disk.
// Values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".
type ResourceList map[string]string
