package v1

import "time"

// APIVersion is the versioned identifier used in YAML manifests.
const APIVersion = "caravanserai/v1"

// TypeMeta holds the Kind and APIVersion fields present in every resource.
type TypeMeta struct {
	// APIVersion identifies the versioned schema this resource conforms to, e.g. "caravanserai/v1".
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`

	// Kind is the resource type, e.g. "Node", "Project".
	Kind string `json:"kind" yaml:"kind"`
}

// ObjectMeta holds identity and classification metadata common to all resources.
type ObjectMeta struct {
	// Name is the unique identifier within its Kind namespace.
	Name string `json:"name" yaml:"name"`

	// Namespace scopes this resource within its Kind. 1.0 locks every write to
	// "" or "default" (enforced in ValidateNamespace); see api/v1/validation.go
	// for the removal pointer once namespaces open up post-1.0.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// ResourceVersion is set by the server and incremented on every write. It
	// is currently a pure write-counter; optimistic-concurrency conflict
	// checking is a separate post-1.0 task.
	ResourceVersion int64 `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`

	// Labels are arbitrary key/value pairs used for selection and grouping.
	Labels map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`

	// Annotations are non-identifying metadata (e.g. human-readable hints).
	Annotations map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`

	// CreatedAt is set by the server on first write.
	CreatedAt time.Time `json:"createdAt,omitempty" yaml:"createdAt,omitempty"`

	// UpdatedAt is set by the server on every write.
	UpdatedAt time.Time `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
}

// ResourceList is a named set of resource quantities — cpu, memory, disk — whose values follow the same string format as Kubernetes: "500m", "4Gi", "100Mbps".
type ResourceList map[string]string
