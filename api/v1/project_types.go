package v1

import (
	"time"

	"github.com/invopop/jsonschema"
)

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

// JSONSchema returns a JSON Schema with the allowed ProjectPhase values.
func (ProjectPhase) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(ProjectPhasePending),
			string(ProjectPhaseScheduled),
			string(ProjectPhaseRunning),
			string(ProjectPhaseFailed),
			string(ProjectPhaseTerminating),
			string(ProjectPhaseTerminated),
		},
		Description: "Lifecycle state of a Project as maintained by the Controller Manager.",
	}
}

// AssignmentHistory records whether a Project has ever held a Node assignment,
// and how confidently that is known. It exists because nodeRef alone cannot
// answer "was this Project ever assigned?": an empty nodeRef looks identical
// whether the Project was never scheduled or was assigned and later released.
// The distinction gates destructive actions — only a Project that was provably
// never assigned may be hard-deleted without first fencing possible runtime
// leftovers on some Node.
type AssignmentHistory string

const (
	// AssignmentHistoryNeverAssigned means the Project has never been granted a
	// Node assignment. It is the only history that permits direct deletion,
	// because no Agent can be holding runtime resources for it.
	AssignmentHistoryNeverAssigned AssignmentHistory = "NeverAssigned"

	// AssignmentHistoryKnown means the Project has been assigned at least once
	// and its assignment generation is a trustworthy fence. New assignments and
	// stale-report rejection rely on this being accurate.
	AssignmentHistoryKnown AssignmentHistory = "Known"

	// AssignmentHistoryUnknown means the Project's assignment history could not
	// be proven — typically an existing row migrated in without enough evidence
	// to declare it NeverAssigned. It is treated conservatively: ineligible for
	// immediate deletion or automatic takeover until cleanup or fencing evidence
	// resolves it to Known or NeverAssigned.
	AssignmentHistoryUnknown AssignmentHistory = "Unknown"
)

// IsValid reports whether h is one of the recognised AssignmentHistory
// constants. The empty string is not valid: every Project row carries an
// explicit history once migration 005 has run.
func (h AssignmentHistory) IsValid() bool {
	switch h {
	case AssignmentHistoryNeverAssigned,
		AssignmentHistoryKnown,
		AssignmentHistoryUnknown:
		return true
	default:
		return false
	}
}

// JSONSchema returns a JSON Schema with the allowed AssignmentHistory values.
func (AssignmentHistory) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(AssignmentHistoryNeverAssigned),
			string(AssignmentHistoryKnown),
			string(AssignmentHistoryUnknown),
		},
		Description: "Whether a Project has ever held a Node assignment, and how confidently that is known.",
	}
}

// EnvVar is a single environment variable to inject into a container.
// Exactly one of Value or ValueFrom may be set (enforced at apply time).
type EnvVar struct {
	Name  string `json:"name"            yaml:"name"`
	Value string `json:"value,omitempty" yaml:"value,omitempty"`

	// ValueFrom sources the value from another object instead of a literal.
	// The agent resolves it to a plain Value before the spec reaches the
	// container runtime; the Runtime layer never sees a ValueFrom.
	ValueFrom *EnvVarSource `json:"valueFrom,omitempty" yaml:"valueFrom,omitempty"`
}

// EnvVarSource holds the possible indirect sources for an EnvVar value.
// Only secretKeyRef exists in 1.0; the wrapper struct mirrors Kubernetes so
// future sources (configMapKeyRef, fieldRef, …) slot in without breaking the
// manifest shape.
type EnvVarSource struct {
	SecretKeyRef *SecretKeySelector `json:"secretKeyRef,omitempty" yaml:"secretKeyRef,omitempty"`
}

// SecretKeySelector points at a single key inside a named Secret.
type SecretKeySelector struct {
	// Name is the Secret's metadata.name.
	Name string `json:"name" yaml:"name"`

	// Key selects one entry in the Secret's spec.data.
	Key string `json:"key" yaml:"key"`
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

	// Command overrides the image's CMD, as in Docker Compose; the image's
	// ENTRYPOINT still applies. Empty keeps the image's default.
	Command []string `json:"command,omitempty" yaml:"command,omitempty"`

	// Env are extra environment variables injected at runtime.
	Env []EnvVar `json:"env,omitempty" yaml:"env,omitempty"`

	// VolumeMounts lists volumes to attach to this container.
	VolumeMounts []VolumeMount `json:"volumeMounts,omitempty" yaml:"volumeMounts,omitempty"`
}

// VolumeType governs the lifecycle and backup behaviour of a Volume.
type VolumeType string

const (
	// VolumeTypeManaged means Caravanserai owns the full lifecycle:
	// provisioning, backup, restore, and deletion. The agent derives the
	// host bind directory from (namespace, project, volume); it is never
	// user-supplied.
	VolumeTypeManaged VolumeType = "Managed"

	// VolumeTypeEphemeral means the volume is discarded when the Project
	// is stopped or moved. No backup or restore occurs.
	VolumeTypeEphemeral VolumeType = "Ephemeral"

	//// VolumeTypeHostPath mounts a path from the Node's filesystem directly.
	//// Reserved for privileged workloads such as monitoring agents.
	//VolumeTypeHostPath VolumeType = "HostPath"
)

// JSONSchema returns a JSON Schema with the allowed VolumeType values.
func (VolumeType) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Enum:        []any{string(VolumeTypeManaged), string(VolumeTypeEphemeral)},
		Description: "Governs the lifecycle and backup behaviour of a Volume.",
	}
}

// VolumeOnMissing selects what the agent does when a Managed volume has no
// backup in the object store at restore time.
type VolumeOnMissing string

const (
	// VolumeOnMissingInitializeEmpty starts the Project with a freshly
	// created empty volume directory when no backup exists.
	VolumeOnMissingInitializeEmpty VolumeOnMissing = "InitializeEmpty"
)

// JSONSchema returns a JSON Schema with the allowed VolumeOnMissing values.
func (VolumeOnMissing) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Enum:        []any{string(VolumeOnMissingInitializeEmpty)},
		Description: "What to do when a Managed volume has no backup at restore time.",
	}
}

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

// JSONSchema returns a JSON Schema with the allowed IngressScope values.
func (IngressScope) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type:        "string",
		Enum:        []any{string(IngressScopeInternal)},
		Description: "Controls whether a route is exposed to the public internet or only to the Headscale overlay network.",
	}
}

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

// TakeoverOutcome is what an operator takeover changed.
type TakeoverOutcome string

const (
	// TakeoverReleased: the assignment was still held and is now released.
	TakeoverReleased TakeoverOutcome = "AssignmentReleased"
	// TakeoverObligationOverridden: the assignment was already released and
	// its obligation was blocking the next assignment.
	TakeoverObligationOverridden TakeoverOutcome = "ObligationOverridden"
)

// TakeoverAuditRecord is one operator takeover decision. It outlives the
// Project it is about.
type TakeoverAuditRecord struct {
	RequestID   string             `json:"requestID" yaml:"requestID"`
	Namespace   string             `json:"namespace" yaml:"namespace"`
	ProjectName string             `json:"projectName" yaml:"projectName"`
	Assignment  AssignmentIdentity `json:"assignment" yaml:"assignment"`
	// Evidence is the machine evidence available when the decision was made:
	// "WorkloadStopped" or "None".
	Evidence string          `json:"evidence" yaml:"evidence"`
	Outcome  TakeoverOutcome `json:"outcome" yaml:"outcome"`
	// Caller is what the server observed about the request. It does not
	// identify a person.
	Caller    string    `json:"caller" yaml:"caller"`
	Note      string    `json:"note,omitempty" yaml:"note,omitempty"`
	CreatedAt time.Time `json:"createdAt" yaml:"createdAt"`
}

// ProjectBackupConfig configures periodic object-store backup of a Project's
// Managed volumes.
//
// The policy lives on the Project rather than on individual volumes because a
// backup run is a single generation: the whole Project is stopped and every
// Managed volume is captured and committed together. Per-volume intervals
// could not be honoured without producing generations whose volumes are from
// different points in time — exactly what stopping the whole Project avoids.
type ProjectBackupConfig struct {
	// Interval is the time between backup runs, e.g. "168h", "1h". Parsed with
	// time.ParseDuration; must be positive.
	Interval string `json:"interval" yaml:"interval"`

	// OnMissing selects behaviour when no backup exists at restore time.
	// Defaults to InitializeEmpty when empty.
	OnMissing VolumeOnMissing `json:"onMissing,omitempty" yaml:"onMissing,omitempty"`
}

// ProjectSize is a t-shirt size declaring a Project's resource footprint. Each
// size maps to fixed CPU and memory amounts (see Requests) that the scheduler
// books against a Node's Allocatable, so users pick a tier instead of
// measuring exact requests.
type ProjectSize string

const (
	ProjectSizeSmall  ProjectSize = "Small"
	ProjectSizeMedium ProjectSize = "Medium"
	ProjectSizeLarge  ProjectSize = "Large"
)

// DefaultProjectSize is the size assumed for a Project that does not declare
// one. It is applied when the size is read (see ProjectSpec.EffectiveSize),
// not stored, so Projects created before the field existed need no backfill.
const DefaultProjectSize = ProjectSizeMedium

// ResourceRequests is the CPU and memory a Project books on its Node.
type ResourceRequests struct {
	CPUMilli    int64
	MemoryBytes int64
}

// projectSizeRequests maps each size to the resources it books. The amounts
// are interim values pending product sign-off (docs/scheduler-strategy.md §3.4).
var projectSizeRequests = map[ProjectSize]ResourceRequests{
	ProjectSizeSmall:  {CPUMilli: 500, MemoryBytes: 512 << 20},
	ProjectSizeMedium: {CPUMilli: 1000, MemoryBytes: 2 << 30},
	ProjectSizeLarge:  {CPUMilli: 2000, MemoryBytes: 4 << 30},
}

// IsValid reports whether s is one of the defined sizes. The empty size is not
// valid here; callers that accept an omitted size check for it separately.
func (s ProjectSize) IsValid() bool {
	_, ok := projectSizeRequests[s]
	return ok
}

// Requests returns the resources a Project of this size books. An empty or
// unknown size books the DefaultProjectSize amounts.
func (s ProjectSize) Requests() ResourceRequests {
	if r, ok := projectSizeRequests[s]; ok {
		return r
	}
	return projectSizeRequests[DefaultProjectSize]
}

// JSONSchema returns a JSON Schema with the allowed ProjectSize values.
func (ProjectSize) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "string",
		Enum: []any{
			string(ProjectSizeSmall),
			string(ProjectSizeMedium),
			string(ProjectSizeLarge),
		},
		Description: "T-shirt size declaring the Project's resource footprint. Defaults to Medium when omitted.",
	}
}

// ProjectSpec is the desired state declared by the user.
type ProjectSpec struct {
	// Size declares the Project's resource footprint for capacity-aware
	// placement. Omitting it means DefaultProjectSize; see EffectiveSize.
	Size ProjectSize `json:"size,omitempty" yaml:"size,omitempty"`

	// Services is the ordered list of containers to run.
	Services []ServiceDef `json:"services" yaml:"services"`

	// Volumes are named storage units shared across services.
	Volumes []VolumeDef `json:"volumes,omitempty" yaml:"volumes,omitempty"`

	// Backup configures periodic backup of every Managed volume in this
	// Project. Omitting it means the volumes persist on the node but are
	// never uploaded.
	Backup *ProjectBackupConfig `json:"backup,omitempty" yaml:"backup,omitempty"`

	// Ingress defines public or internal HTTP routing rules.
	Ingress []IngressDef `json:"ingress,omitempty" yaml:"ingress,omitempty"`

	// ExpireAt, when set, causes the GC controller to delete the Project
	// after this time. Useful for ephemeral preview environments.
	ExpireAt *time.Time `json:"expireAt,omitempty" yaml:"expireAt,omitempty"`
}

// EffectiveSize returns the declared Size, or DefaultProjectSize when none was
// declared.
func (s ProjectSpec) EffectiveSize() ProjectSize {
	if s.Size == "" {
		return DefaultProjectSize
	}
	return s.Size
}

// InitialProjectStatus is the status every Project is created with: Pending,
// generation zero, NeverAssigned, and nothing else. It is a whole value rather
// than a list of fields to clear, so a field added to ProjectStatus later starts
// empty on create without anyone remembering to reset it.
func InitialProjectStatus() ProjectStatus {
	return ProjectStatus{
		Phase:                ProjectPhasePending,
		AssignmentGeneration: 0,
		AssignmentHistory:    AssignmentHistoryNeverAssigned,
	}
}

// ProjectStatus is written by the Controller Manager and Agent.
type ProjectStatus struct {
	// Phase is the high-level lifecycle state.
	Phase ProjectPhase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// NodeRef is the name of the Node the Scheduler chose. Empty while Pending.
	NodeRef string `json:"nodeRef,omitempty" yaml:"nodeRef,omitempty"`

	// AssignmentGeneration is a server-owned counter that increments by one on
	// every successful Pending-to-Scheduled assignment. Together with UID and
	// nodeRef it forms the fence an Agent must present on every write: a report
	// carrying an older generation is rejected as stale, which defeats the ABA
	// problem that UID and nodeRef alone cannot — a Project reassigned A→B→A
	// reuses the Node name but never a generation. Clearing nodeRef revokes the
	// current generation but does not reset the counter.
	//
	// This is not a user manifest field; clients cannot set it.
	AssignmentGeneration int64 `json:"assignmentGeneration,omitempty" yaml:"assignmentGeneration,omitempty"`

	// AssignmentHistory records whether this Project was ever assigned a Node,
	// gating whether it may be hard-deleted directly or must enter the cleanup
	// lifecycle first. Server-owned; not a user manifest field.
	AssignmentHistory AssignmentHistory `json:"assignmentHistory,omitempty" yaml:"assignmentHistory,omitempty"`

	// FailedNodes names the Nodes this Project was moved off after it was
	// reported Failed, oldest first. The scheduler never places the Project on
	// one of them, so a move actually changes Node, and its length is how many
	// moves have been spent against the retry cap.
	//
	// It is cleared when the Project has run on a new Node long enough to count
	// as recovered, and when the user applies the Project again.
	//
	// Server-owned; not a user manifest field.
	FailedNodes []string `json:"failedNodes,omitempty" yaml:"failedNodes,omitempty"`

	// Conditions is a list of granular observable states.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`

	// Recovery is the durable state of cross-Node recovery: retry bookkeeping
	// and the evidence that lets an assignment be released or a record be
	// deleted. It lives here rather than in Condition messages because it must
	// survive server restarts and duplicate controller events, and because code
	// must never recover a counter or an identity by parsing a message.
	//
	// Server-owned; not a user manifest field. Nil until recovery first records
	// something.
	Recovery *ProjectRecoveryStatus `json:"recovery,omitempty" yaml:"recovery,omitempty"`
}

// ProjectRecoveryStatus is the structured recovery state of a Project.
//
// The retry, backoff, preference and healthy-window fields are written only by
// server-side controllers. The evidence fields are written only through
// dedicated guarded mutations that validate the reporting Agent's identity; an
// ordinary Agent status report can set or clear none of them.
type ProjectRecoveryStatus struct {
	// RescheduleAttempts counts cross-Node reschedules in the current incident.
	RescheduleAttempts int `json:"rescheduleAttempts,omitempty" yaml:"rescheduleAttempts,omitempty"`

	// LastAttemptAt is when the most recent reschedule was attempted. Backoff
	// is computed from it.
	LastAttemptAt *time.Time `json:"lastAttemptAt,omitempty" yaml:"lastAttemptAt,omitempty"`

	// HealthySince is when the Project was last seen continuously healthy. It
	// is meaningful only while HealthySinceGeneration equals the current
	// assignment generation: a window observed under an older assignment says
	// nothing about the current one.
	HealthySince *time.Time `json:"healthySince,omitempty" yaml:"healthySince,omitempty"`

	// HealthySinceGeneration is the assignment generation HealthySince was
	// observed under.
	HealthySinceGeneration int64 `json:"healthySinceGeneration,omitempty" yaml:"healthySinceGeneration,omitempty"`

	// PreferredNode is a scheduler hint only. Status.NodeRef remains the
	// authoritative assignment.
	PreferredNode string `json:"preferredNode,omitempty" yaml:"preferredNode,omitempty"`

	// WorkloadStopped is the durable no-start latch for one assignment
	// generation: the assigned Agent confirmed that every Project container is
	// stopped. It is not proof that runtime resources were removed, and it
	// alone never authorizes final deletion. Only a committed newer assignment
	// clears it; time alone does not.
	WorkloadStopped *WorkloadStoppedEvidence `json:"workloadStopped,omitempty" yaml:"workloadStopped,omitempty"`

	// CleanupObligations lists every assignment this Project has given up
	// whose Node may still hold its runtime resources. An entry is appended in
	// the same write that releases an assignment or moves the Project to
	// Terminating, so the list is the complete set of Nodes that may still
	// hold resources. Final deletion requires every entry to be resolved.
	CleanupObligations []CleanupObligation `json:"cleanupObligations,omitempty" yaml:"cleanupObligations,omitempty"`

	// CommittedBackup is the generation a new placement restores from
	// (ADR-0024 C1). Only the assignment current at the time can set it, so once
	// an assignment is released its Agent can no longer change where the next
	// one restores from, even if it can still reach the object store. A new
	// assignment keeps it. Nil means no backup has ever been committed.
	CommittedBackup *CommittedBackup `json:"committedBackup,omitempty" yaml:"committedBackup,omitempty"`
}

// CommittedBackup records one backup generation accepted as the restore source.
type CommittedBackup struct {
	// BackupID names the generation in the object store.
	BackupID string `json:"backupID" yaml:"backupID"`
	// Assignment is the assignment that produced and committed it.
	Assignment AssignmentIdentity `json:"assignment" yaml:"assignment"`
	// CommittedAt is when the server accepted it.
	CommittedAt time.Time `json:"committedAt" yaml:"committedAt"`
}

// CommittedBackupID returns the generation a new placement restores from, or
// "" if no backup has been committed. It is safe on a nil receiver.
func (r *ProjectRecoveryStatus) CommittedBackupID() string {
	if r == nil || r.CommittedBackup == nil {
		return ""
	}
	return r.CommittedBackup.BackupID
}

// ReleaseAuthorizationKind records what permitted an assignment to be given
// up. It answers "why could another Node take over", never "has this Node
// cleaned up" — that is ResourcesRemoved, recorded separately.
type ReleaseAuthorizationKind string

const (
	// ReleaseByWorkloadStopped: the assigned Agent confirmed every container
	// of that generation stopped.
	ReleaseByWorkloadStopped ReleaseAuthorizationKind = "WorkloadStopped"
	// ReleaseByDeletion: the Project entered Terminating; the assignment's
	// runtime resources are to be removed, not handed to another Node.
	ReleaseByDeletion ReleaseAuthorizationKind = "Deletion"
	// ReleaseByProviderFencing: a provider API proved the Node powered off or
	// isolated. Not implemented yet.
	ReleaseByProviderFencing ReleaseAuthorizationKind = "ProviderFencing"
	// ReleaseByOperatorOverride: an operator accepted the risk of takeover
	// without machine evidence. Never fencing or cleanup evidence: the old
	// Node still owes cleanup if it ever returns.
	ReleaseByOperatorOverride ReleaseAuthorizationKind = "OperatorOverride"
	// ReleaseByGraceExpired: the Node stayed NotReady past its grace period and
	// the Project has no Managed volume, so there is no data to diverge. The
	// transitional authorization of ADR-0024: a partitioned Node keeps running
	// its copy until it reconnects. Never evidence the workload stopped.
	ReleaseByGraceExpired ReleaseAuthorizationKind = "GraceExpired"
	// ReleaseByLeaseExpired: the Node's lease expired under the self-fencing
	// contract proposed in ADR-0023. Not implemented yet; never cleanup
	// evidence.
	ReleaseByLeaseExpired ReleaseAuthorizationKind = "LeaseExpired"
)

// CleanupObligation is one released assignment whose Node may still hold the
// Project's runtime resources.
type CleanupObligation struct {
	// Assignment is the released assignment. Its Agent is the cleanup owner.
	Assignment AssignmentIdentity `json:"assignment" yaml:"assignment"`

	// ReleasedAt is when the assignment was given up.
	ReleasedAt time.Time `json:"releasedAt" yaml:"releasedAt"`

	// ReleaseAuthorization is what permitted giving the assignment up.
	ReleaseAuthorization ReleaseAuthorizationKind `json:"releaseAuthorization" yaml:"releaseAuthorization"`

	// ResourcesRemoved is the cleanup owner's evidence that every
	// deletion-scoped runtime resource it held for this assignment is gone.
	// Nil means the obligation is unresolved.
	ResourcesRemoved *ResourcesRemovedEvidence `json:"resourcesRemoved,omitempty" yaml:"resourcesRemoved,omitempty"`
}

// Resolved reports whether the cleanup owner has reported ResourcesRemoved.
func (o CleanupObligation) Resolved() bool {
	return o.ResourcesRemoved != nil
}

// FindObligation returns the obligation for assignment, or nil.
func (r *ProjectRecoveryStatus) FindObligation(assignment AssignmentIdentity) *CleanupObligation {
	if r == nil {
		return nil
	}
	for i := range r.CleanupObligations {
		if r.CleanupObligations[i].Assignment == assignment {
			return &r.CleanupObligations[i]
		}
	}
	return nil
}

// AddObligation records that assignment was given up under authorization and
// its Node now owes cleanup. An assignment already in the list keeps its
// original entry: an obligation records the first release, and re-releasing
// the same assignment must not reset its evidence.
func (r *ProjectRecoveryStatus) AddObligation(assignment AssignmentIdentity, releasedAt time.Time, authorization ReleaseAuthorizationKind) {
	if r.FindObligation(assignment) != nil {
		return
	}
	r.CleanupObligations = append(r.CleanupObligations, CleanupObligation{
		Assignment:           assignment,
		ReleasedAt:           releasedAt,
		ReleaseAuthorization: authorization,
	})
}

// OverrideRelease records an operator's takeover of assignment. An obligation
// already standing for it, from a WorkloadStopped release whose Node never
// came back to clean up, changes authorization rather than gaining a second
// entry. Either way it stays unresolved: the override permits a new assignment
// and says nothing about what the old Node still holds.
func (r *ProjectRecoveryStatus) OverrideRelease(assignment AssignmentIdentity, releasedAt time.Time) {
	if o := r.FindObligation(assignment); o != nil {
		o.ReleaseAuthorization = ReleaseByOperatorOverride
		return
	}
	r.AddObligation(assignment, releasedAt, ReleaseByOperatorOverride)
}

// NodesOwingCleanup returns the Nodes with an unresolved obligation.
func (r *ProjectRecoveryStatus) NodesOwingCleanup() []string {
	if r == nil {
		return nil
	}
	var nodes []string
	for _, o := range r.CleanupObligations {
		if !o.Resolved() {
			nodes = append(nodes, o.Assignment.NodeName)
		}
	}
	return nodes
}

// HasManagedVolume reports whether the spec declares a Managed volume.
func (s ProjectSpec) HasManagedVolume() bool {
	for _, v := range s.Volumes {
		if v.Type == VolumeTypeManaged {
			return true
		}
	}
	return false
}

// PermitsAssignment reports whether this obligation lets the Project be
// assigned again. A resolved obligation does. An unresolved one does only when
// its release was authorized by evidence that the old Node can no longer run
// the workload — provider fencing or an audited operator override. A
// WorkloadStopped release does not: the containers are stopped but still
// present, and a restarted Agent or Docker restart policy could bring them back
// while the new assignment runs.
//
// LeaseExpired is refused only because no lease is implemented yet and nothing
// can record that kind. It is not a decision: the lease design separates
// "another Node may take over" from "the old Node still owes cleanup", so its
// answer here is to be set when the lease itself is implemented.
func (o CleanupObligation) PermitsAssignment() bool {
	if o.Resolved() {
		return true
	}
	switch o.ReleaseAuthorization {
	case ReleaseByProviderFencing, ReleaseByOperatorOverride, ReleaseByGraceExpired:
		return true
	default:
		return false
	}
}

// BlockingObligation returns the first obligation that forbids assigning the
// Project, or nil when none does.
func (r *ProjectRecoveryStatus) BlockingObligation() *CleanupObligation {
	if r == nil {
		return nil
	}
	for i := range r.CleanupObligations {
		if !r.CleanupObligations[i].PermitsAssignment() {
			return &r.CleanupObligations[i]
		}
	}
	return nil
}

// ResourcesRemovedFor reports whether the cleanup obligation of the assignment
// held on node at generation carries ResourcesRemoved. It is what lets that
// assignment's Agent report the Project Terminated: the phase may say the
// resources are gone only after the evidence that they are has been recorded.
//
// It matches by Node and generation rather than by full identity because the
// list belongs to one Project record, and create resets status, so every entry
// in it is of that record's lifetime.
func (r *ProjectRecoveryStatus) ResourcesRemovedFor(node string, generation int64) bool {
	if r == nil {
		return false
	}
	for _, o := range r.CleanupObligations {
		if o.Assignment.NodeName == node && o.Assignment.AssignmentGeneration == generation {
			return o.Resolved()
		}
	}
	return false
}

// WorkloadStoppedFor reports whether WorkloadStopped evidence is recorded for
// exactly assignment. Evidence for any other generation, Node or lifetime says
// nothing about this one.
func (r *ProjectRecoveryStatus) WorkloadStoppedFor(assignment AssignmentIdentity) bool {
	return r != nil && r.WorkloadStopped != nil && r.WorkloadStopped.Assignment == assignment
}

// EnsureRecovery returns s.Recovery, allocating it first if needed.
func (s *ProjectStatus) EnsureRecovery() *ProjectRecoveryStatus {
	if s.Recovery == nil {
		s.Recovery = &ProjectRecoveryStatus{}
	}
	return s.Recovery
}

// AssignmentIdentity names one assignment of one Project lifetime. All three
// parts are needed: UID separates lifetimes that share a name, and the
// generation separates assignments that reuse a Node (A→B→A).
type AssignmentIdentity struct {
	ProjectUID           string `json:"projectUID" yaml:"projectUID"`
	NodeName             string `json:"nodeName" yaml:"nodeName"`
	AssignmentGeneration int64  `json:"assignmentGeneration" yaml:"assignmentGeneration"`
}

// WorkloadStoppedEvidence records that the Agent of Assignment confirmed every
// Project container stopped. ObservedAt is when that verification completed.
type WorkloadStoppedEvidence struct {
	Assignment AssignmentIdentity `json:"assignment" yaml:"assignment"`
	ObservedAt time.Time          `json:"observedAt" yaml:"observedAt"`
}

// ResourcesRemovedEvidence records when the cleanup owner of an obligation
// finished removing every deletion-scoped runtime resource it held. The
// assignment it is about is the obligation's own.
type ResourcesRemovedEvidence struct {
	ObservedAt time.Time `json:"observedAt" yaml:"observedAt"`
}

// ClearForNewAssignment drops the state a newly committed assignment
// invalidates: the previous generation's no-start latch and its healthy
// window. It is the only operation that clears WorkloadStopped, and it must run
// inside the same compare-and-swap that commits the new assignment.
func (r *ProjectRecoveryStatus) ClearForNewAssignment() {
	if r == nil {
		return
	}
	r.WorkloadStopped = nil
	r.ClearHealthyWindow()
}

// ClearHealthyWindow forgets the current healthy window, as any new failure or
// assignment must.
func (r *ProjectRecoveryStatus) ClearHealthyWindow() {
	if r == nil {
		return
	}
	r.HealthySince = nil
	r.HealthySinceGeneration = 0
}

// DeepCopy returns a ProjectRecoveryStatus sharing no pointer with r.
func (r *ProjectRecoveryStatus) DeepCopy() *ProjectRecoveryStatus {
	if r == nil {
		return nil
	}
	out := *r
	out.LastAttemptAt = copyTime(r.LastAttemptAt)
	out.HealthySince = copyTime(r.HealthySince)
	if r.WorkloadStopped != nil {
		ws := *r.WorkloadStopped
		out.WorkloadStopped = &ws
	}
	if r.CommittedBackup != nil {
		cb := *r.CommittedBackup
		out.CommittedBackup = &cb
	}
	if r.CleanupObligations != nil {
		out.CleanupObligations = make([]CleanupObligation, len(r.CleanupObligations))
		for i, o := range r.CleanupObligations {
			if o.ResourcesRemoved != nil {
				rr := *o.ResourcesRemoved
				o.ResourcesRemoved = &rr
			}
			out.CleanupObligations[i] = o
		}
	}
	return &out
}

func copyTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
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

// JSONSchemaExtend sets the top-level title for the Project schema.
func (Project) JSONSchemaExtend(s *jsonschema.Schema) {
	s.Title = "Project"
}

// ProjectList is a collection of Project objects returned by list operations.
type ProjectList struct {
	TypeMeta `json:",inline"  yaml:",inline"`
	Items    []Project `json:"items" yaml:"items"`
}

// CleanupTask is one unresolved cleanup obligation, addressed to the Agent of
// the Node that owes it. It authorizes that Agent to stop and remove the
// runtime resources of exactly Assignment and to report ResourcesRemoved for
// it; it grants no status-write or workload-start authority.
type CleanupTask struct {
	Namespace            string                   `json:"namespace" yaml:"namespace"`
	ProjectName          string                   `json:"projectName" yaml:"projectName"`
	Assignment           AssignmentIdentity       `json:"assignment" yaml:"assignment"`
	ReleaseAuthorization ReleaseAuthorizationKind `json:"releaseAuthorization" yaml:"releaseAuthorization"`
}

// CleanupTaskList is the response of the Node cleanup-obligation listing.
type CleanupTaskList struct {
	TypeMeta `json:",inline" yaml:",inline"`
	Items    []CleanupTask `json:"items" yaml:"items"`
}

// CleanupTasksFor returns p's unresolved cleanup obligations owed by node, as
// tasks for that Node's Agent.
//
// None is returned while node holds p's current assignment. That Node's Agent
// is acting on the Project under its assignment — tearing it down if it is
// Terminating — and a cleanup task running beside it could remove what the
// assignment is still using. The Deletion obligation of a Terminating Project
// is resolved by the teardown itself, not by a task.
//
// An obligation for another lifetime cannot be in p's list (the UID check
// guards every write to it), but the UID is compared anyway: a task is only
// ever about the lifetime it names.
func (p *Project) CleanupTasksFor(node string) []CleanupTask {
	if p.Status.Recovery == nil || p.Status.NodeRef == node {
		return nil
	}
	namespace := p.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	var tasks []CleanupTask
	for _, o := range p.Status.Recovery.CleanupObligations {
		if o.Resolved() || o.Assignment.NodeName != node || o.Assignment.ProjectUID != p.ObjectMeta.UID {
			continue
		}
		tasks = append(tasks, CleanupTask{
			Namespace:            namespace,
			ProjectName:          p.Name,
			Assignment:           o.Assignment,
			ReleaseAuthorization: o.ReleaseAuthorization,
		})
	}
	return tasks
}
