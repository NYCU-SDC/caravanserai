package v1

import "time"

// ProjectPhase is the lifecycle state of a Project as maintained by the
// Controller Manager.
type ProjectPhase string

const (
	// ProjectPhasePending means the Project has been accepted by the API
	// server but the Scheduler has not yet assigned it to a Node.
	ProjectPhasePending ProjectPhase = "Pending"

	// ProjectPhaseScheduled means the Scheduler has chosen a target Node
	// and written the nodeRef. The Agent has not yet confirmed running.
	ProjectPhaseScheduled ProjectPhase = "Scheduled"

	// ProjectPhaseRunning means the Agent has confirmed all containers are up.
	ProjectPhaseRunning ProjectPhase = "Running"

	// ProjectPhaseFailed means the Agent could not start the Project or
	// reported a terminal error.
	ProjectPhaseFailed ProjectPhase = "Failed"

	// ProjectPhaseTerminating means a deletion has been requested and the
	// Agent is tearing down containers.
	ProjectPhaseTerminating ProjectPhase = "Terminating"

	// ProjectPhaseTerminated means the Agent has finished removing all
	// containers and Docker resources. The ProjectTerminationController will
	// delete the record from the store shortly after this phase is observed.
	ProjectPhaseTerminated ProjectPhase = "Terminated"
)

// IsValid reports whether p is one of the recognised ProjectPhase constants.
func (p ProjectPhase) IsValid() bool {
	switch p {
	case ProjectPhasePending,
		ProjectPhaseScheduled,
		ProjectPhaseRunning,
		ProjectPhaseFailed,
		ProjectPhaseTerminating,
		ProjectPhaseTerminated:
		return true
	default:
		return false
	}
}

// EnvVar is a single environment variable to inject into a container.
type EnvVar struct {
	Name  string `json:"name"            yaml:"name"`
	Value string `json:"value,omitempty" yaml:"value,omitempty"`
}

// VolumeMount associates a named Volume with a container mount path.
type VolumeMount struct {
	Name      string `json:"name"      yaml:"name"`
	MountPath string `json:"mountPath" yaml:"mountPath"`
}

// ServiceDef describes a single container (analogous to a Compose service).
type ServiceDef struct {
	// Name identifies the service within the Project (used as the DNS
	// hostname inside the shared bridge network).
	Name string `json:"name" yaml:"name"`

	// Image is the Docker image reference, e.g. "postgres:15".
	Image string `json:"image" yaml:"image"`

	// Env are extra environment variables injected at runtime.
	Env []EnvVar `json:"env,omitempty" yaml:"env,omitempty"`

	// VolumeMounts lists volumes to attach to this container.
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty" yaml:"volumeMounts,omitempty"`
}

// VolumeType governs the lifecycle and backup behaviour of a Volume.
type VolumeType string

const (
	//// VolumeTypeManaged means Caravanserai owns the full lifecycle:
	//// provisioning, backup, restore, and deletion.
	//VolumeTypeManaged VolumeType = "Managed"

	// VolumeTypeEphemeral means the volume is discarded when the Project
	// is stopped or moved. No backup or restore occurs.
	VolumeTypeEphemeral VolumeType = "Ephemeral"

	//// VolumeTypeHostPath mounts a path from the Node's filesystem directly.
	//// Reserved for privileged workloads such as monitoring agents.
	//VolumeTypeHostPath VolumeType = "HostPath"
)

// VolumeDef describes a named volume used by one or more services.
type VolumeDef struct {
	Name string `json:"name" yaml:"name"`

	// Type determines lifecycle and backup semantics.
	Type VolumeType `json:"type" yaml:"type"`
}

// IngressScope controls whether a route is exposed to the public internet
// (via Cloudflare Tunnel) or only to the Headscale overlay network.
type IngressScope string

const (
	// Todo: add "Public" scope and implement Cloudflare Tunnel integration.
	//IngressScopePublic   IngressScope = "Public"

	IngressScopeInternal IngressScope = "Internal"
)

// IngressTarget is the backend service and port for an ingress rule.
type IngressTarget struct {
	Service string `json:"service" yaml:"service"`
	Port    int    `json:"port"    yaml:"port"`
}

// IngressAccess defines visibility and auth rules for an ingress endpoint.
type IngressAccess struct {
	Scope IngressScope `json:"scope" yaml:"scope"`
}

// IngressDef describes a single HTTP ingress rule for a Project.
//
// If Host contains a dot it is used verbatim; otherwise the final hostname
// is assembled as: {host}.{environment}.{baseDomain}.
type IngressDef struct {
	Name   string        `json:"name"             yaml:"name"`
	Host   string        `json:"host,omitempty"   yaml:"host,omitempty"`
	Target IngressTarget `json:"target"           yaml:"target"`
	Access IngressAccess `json:"access,omitempty" yaml:"access,omitempty"`
}

// ProjectSpec is the desired state declared by the user.
type ProjectSpec struct {
	// Services is the ordered list of containers to run.
	Services []ServiceDef `json:"services" yaml:"services"`

	// Volumes are named storage units shared across services.
	Volumes []VolumeDef `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// Ingress defines public or internal HTTP routing rules.
	Ingress []IngressDef `json:"ingress,omitempty" yaml:"ingress,omitempty"`

	// ExpireAt, when set, causes the GC controller to delete the Project
	// after this time. Useful for ephemeral preview environments.
	ExpireAt *time.Time `json:"expireAt,omitempty" yaml:"expireAt,omitempty"`
}

// ProjectStatus is written by the Controller Manager and Agent.
type ProjectStatus struct {
	// Phase is the high-level lifecycle state.
	Phase ProjectPhase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// NodeRef is the name of the Node the Scheduler chose. Empty while Pending.
	NodeRef string `json:"nodeRef,omitempty" yaml:"nodeRef,omitempty"`

	// Conditions is a list of granular observable states.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Project is the central workload resource. It groups one or more containers
// (services) that must be co-located on a single Node. Internal networking
// uses a Docker bridge; service names resolve as hostnames, identical to
// docker compose behaviour.
//
// YAML example:
//
//	apiVersion: caravanserai/v1
//	kind: Project
//	metadata:
//	  name: core-system-prod
//	spec:
//	  resources:
//	    cpu: "2000m"
//	    memory: "4Gi"
//	  services:
//	    - name: backend
//	      image: nycusdc/core-system-backend:latest
type Project struct {
	TypeMeta   `json:",inline"    yaml:",inline"`
	ObjectMeta `json:"metadata"   yaml:"metadata"`
	Spec       ProjectSpec   `json:"spec,omitempty"   yaml:"spec,omitempty"`
	Status     ProjectStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// ProjectList is a collection of Project objects returned by list operations.
type ProjectList struct {
	TypeMeta `json:",inline"  yaml:",inline"`
	Items    []Project `json:"items" yaml:"items"`
}
