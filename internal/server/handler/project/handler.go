// Package project provides the HTTP handler for Project resources.
//
// Routes registered:
//
//	POST   /api/v1/projects                  — create a Project
//	PUT    /api/v1/projects/{name}           — update a Project's spec
//	GET    /api/v1/projects                  — list Projects (supports ?phase= and ?nodeRef= filters)
//	GET    /api/v1/projects/{name}           — get a single Project
//	DELETE /api/v1/projects/{name}           — delete a Project (supports ?force=true for immediate hard-delete)
//	PATCH  /api/v1/projects/{name}/status    — Agent reports observed phase (Running / Failed / Terminated)
//	PATCH  /api/v1/projects/{name}/conditions/{type}  — Agent sets one condition without touching phase
//	DELETE /api/v1/projects/{name}/conditions/{type}  — Agent clears one condition
//	PUT    /api/v1/projects/{name}/recovery/workload-stopped   — assigned Agent records that every container is stopped
//	PUT    /api/v1/projects/{name}/recovery/resources-removed  — cleanup owner records that its resources are removed
//	PUT    /api/v1/projects/{name}/recovery/committed-backup   — assigned Agent makes a backup the restore source
//	PUT    /api/v1/projects/{name}/recovery/managed-data-quarantined — cleanup owner records that its local data left the active path
//	POST   /api/v1/projects/{name}/recovery/takeover           — operator overrides the release gate (operator token)
//	GET    /api/v1/projects/{name}/recovery/takeover-audit     — recorded takeovers of the Project (operator token)
package project

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Handler implements apiserver.RouteRegistrar for Project endpoints.
type Handler struct {
	logger        *zap.Logger
	store         store.ProjectStore
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter

	// operatorToken guards the takeover routes until ADR-0018 settles API
	// access control. Empty disables them.
	operatorToken string

	// postgresImage is the image a PostgresDifferential volume's service must
	// run (see WithPostgresDifferentialImage).
	postgresImage string
}

// NewHandler creates a Project Handler.
func NewHandler(logger *zap.Logger, s store.ProjectStore, pw *problem.HttpWriter, opts ...Option) *Handler {
	h := &Handler{
		logger:        logger,
		store:         s,
		tracer:        otel.Tracer("project/handler"),
		problemWriter: pw,
		postgresImage: v1.PostgresDifferentialImage,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Option configures a Handler.
type Option func(*Handler)

// WithPostgresDifferentialImage replaces the image a PostgresDifferential
// volume's service must use. An empty reference keeps the published one.
func WithPostgresDifferentialImage(ref string) Option {
	return func(h *Handler) {
		if ref != "" {
			h.postgresImage = ref
		}
	}
}

// WithOperatorToken sets the credential the takeover routes require; they stay
// disabled without one.
func (h *Handler) WithOperatorToken(token string) *Handler {
	h.operatorToken = token
	return h
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/projects", mid.HandlerFunc(h.createProject))
	mux.HandleFunc("PUT /api/v1/projects/{name}", mid.HandlerFunc(h.updateProject))
	mux.HandleFunc("GET /api/v1/projects", mid.HandlerFunc(h.listProjects))
	mux.HandleFunc("GET /api/v1/projects/{name}", mid.HandlerFunc(h.getProject))
	mux.HandleFunc("DELETE /api/v1/projects/{name}", mid.HandlerFunc(h.deleteProject))
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", mid.HandlerFunc(h.patchStatus))
	mux.HandleFunc("PATCH /api/v1/projects/{name}/conditions/{type}", mid.HandlerFunc(h.patchCondition))
	mux.HandleFunc("DELETE /api/v1/projects/{name}/conditions/{type}", mid.HandlerFunc(h.deleteCondition))
	mux.HandleFunc("PUT /api/v1/projects/{name}/recovery/workload-stopped", mid.HandlerFunc(h.putWorkloadStopped))
	mux.HandleFunc("PUT /api/v1/projects/{name}/recovery/resources-removed", mid.HandlerFunc(h.putResourcesRemoved))
	mux.HandleFunc("PUT /api/v1/projects/{name}/recovery/committed-backup", mid.HandlerFunc(h.putCommittedBackup))
	mux.HandleFunc("PUT /api/v1/projects/{name}/recovery/managed-data-quarantined", mid.HandlerFunc(h.putManagedDataQuarantined))
	mux.HandleFunc("POST /api/v1/projects/{name}/recovery/takeover", mid.HandlerFunc(h.postTakeover))
	mux.HandleFunc("GET /api/v1/projects/{name}/recovery/takeover-audit", mid.HandlerFunc(h.getTakeoverAudit))
}

// ── handlers ──────────────────────────────────────────────────────────────────

// validateProjectSpec validates the project spec, returning a handlerutil.ValidationError
// if any rule is violated, or nil if the spec is valid.
func validateProjectSpec(spec v1.ProjectSpec) error {
	return validateProjectSpecFor(spec, v1.PostgresDifferentialImage)
}

// validateProjectSpecFor is validateProjectSpec with the image a
// PostgresDifferential volume's service must use.
func validateProjectSpecFor(spec v1.ProjectSpec, postgresImage string) error {
	if len(spec.Services) == 0 {
		return handlerutil.NewValidationError("spec.services", nil, "spec.services must contain at least one service")
	}
	if spec.Size != "" && !spec.Size.IsValid() {
		return handlerutil.NewValidationError("spec.size", spec.Size,
			`spec.size must be one of "Small", "Medium", "Large"`)
	}
	for _, svc := range spec.Services {
		if svc.Name == "" {
			return handlerutil.NewValidationError("spec.services[].name", nil, "each service must have a non-empty name")
		}
		if svc.Image == "" {
			return handlerutil.NewValidationError("spec.services[].image", svc.Name, "service "+svc.Name+": image is required")
		}
		for _, env := range svc.Env {
			if err := validateEnvVar(svc.Name, env); err != nil {
				return err
			}
		}
	}

	volumeNames := make(map[string]bool, len(spec.Volumes))
	managedVolumes := make(map[string]bool, len(spec.Volumes))
	hasManagedVolume := false
	for _, vol := range spec.Volumes {
		if vol.Name == "" {
			return handlerutil.NewValidationError("spec.volumes[].name", nil, "each volume must have a non-empty name")
		}
		if volumeNames[vol.Name] {
			return handlerutil.NewValidationError("spec.volumes[].name", vol.Name, "duplicate volume name: "+vol.Name)
		}
		volumeNames[vol.Name] = true

		switch vol.Type {
		case v1.VolumeTypeManaged:
			hasManagedVolume = true
			managedVolumes[vol.Name] = true
		case v1.VolumeTypeEphemeral:
		default:
			return handlerutil.NewValidationError("spec.volumes[].type", vol.Type,
				"volume "+vol.Name+": type must be \"Managed\" or \"Ephemeral\"")
		}
	}

	if spec.Backup != nil {
		// A backup policy with nothing to back up is always a mistake, and
		// would otherwise be a silent no-op.
		if !hasManagedVolume {
			return handlerutil.NewValidationError("spec.backup", nil,
				"spec.backup requires at least one volume with type \"Managed\"")
		}
		d, err := time.ParseDuration(spec.Backup.Interval)
		if err != nil || d <= 0 {
			return handlerutil.NewValidationError("spec.backup.interval", spec.Backup.Interval,
				"spec.backup.interval must be a positive duration such as \"168h\" or \"1h\"")
		}
		if spec.Backup.OnMissing != "" && spec.Backup.OnMissing != v1.VolumeOnMissingInitializeEmpty {
			return handlerutil.NewValidationError("spec.backup.onMissing", spec.Backup.OnMissing,
				"spec.backup.onMissing must be \"InitializeEmpty\"")
		}
		if err := validateBackupItems(spec, volumeNames, managedVolumes, postgresImage); err != nil {
			return err
		}
	}

	for _, svc := range spec.Services {
		for _, vm := range svc.VolumeMounts {
			if !volumeNames[vm.Name] {
				return handlerutil.NewValidationError("spec.services[].volumeMounts[].name", vm.Name,
					"service "+svc.Name+": volumeMount references undeclared volume "+vm.Name)
			}
			if vm.MountPath == "" {
				return handlerutil.NewValidationError("spec.services[].volumeMounts[].mountPath", vm.Name,
					"service "+svc.Name+": volumeMount "+vm.Name+" must have a non-empty mountPath")
			}
		}
	}

	if len(spec.Ingress) > 0 {
		serviceNames := make(map[string]bool, len(spec.Services))
		for _, svc := range spec.Services {
			serviceNames[svc.Name] = true
		}

		ingressNames := make(map[string]bool, len(spec.Ingress))
		for _, ing := range spec.Ingress {
			if ing.Name == "" {
				return handlerutil.NewValidationError("spec.ingress[].name", nil, "each ingress entry must have a non-empty name")
			}

			if ingressNames[ing.Name] {
				return handlerutil.NewValidationError("spec.ingress[].name", ing.Name, "duplicate ingress name: "+ing.Name)
			}
			ingressNames[ing.Name] = true

			if !serviceNames[ing.Target.Service] {
				return handlerutil.NewValidationError("spec.ingress[].target.service", ing.Target.Service,
					"ingress "+ing.Name+": target service "+ing.Target.Service+" does not exist in spec.services")
			}

			if ing.Target.Port <= 0 {
				return handlerutil.NewValidationError("spec.ingress[].target.port", ing.Target.Port,
					fmt.Sprintf("ingress %s: target port must be greater than 0", ing.Name))
			}

			if ing.Access.Scope != "" && ing.Access.Scope != v1.IngressScopeInternal {
				return handlerutil.NewValidationError("spec.ingress[].access.scope", ing.Access.Scope,
					"ingress "+ing.Name+": only \"Internal\" scope is supported")
			}
		}
	}

	return nil
}

// validateBackupItems checks spec.backup.items against the volumes and services
// they refer to. volumeNames holds every declared volume and managedVolumes the
// Managed ones.
//
// PostgresDifferential is accepted only where Cara can run it: the volume must
// be the PGDATA of exactly one service on PostgresDifferentialImage, because
// Cara starts that PostgreSQL, creates its stanza and enables archiving in it.
// Mixing strategies in one Project is rejected until a backup generation can
// hold both kinds of item.
func validateBackupItems(spec v1.ProjectSpec, volumeNames, managedVolumes map[string]bool, postgresImage string) error {
	seen := make(map[string]bool, len(spec.Backup.Items))
	for _, item := range spec.Backup.Items {
		switch {
		case item.Volume == "":
			return handlerutil.NewValidationError("spec.backup.items[].volume", nil,
				"each backup item must name a volume")
		case !volumeNames[item.Volume]:
			return handlerutil.NewValidationError("spec.backup.items[].volume", item.Volume,
				"backup item references undeclared volume "+item.Volume)
		case !managedVolumes[item.Volume]:
			return handlerutil.NewValidationError("spec.backup.items[].volume", item.Volume,
				"backup item "+item.Volume+": only a Managed volume can have a backup strategy")
		case seen[item.Volume]:
			return handlerutil.NewValidationError("spec.backup.items[].volume", item.Volume,
				"duplicate backup item for volume "+item.Volume)
		}
		seen[item.Volume] = true

		switch item.Strategy {
		case v1.BackupStrategyOffline:
			if item.RetainFull != nil {
				return handlerutil.NewValidationError("spec.backup.items[].retainFull", item.Volume,
					"backup item "+item.Volume+": retainFull applies only to strategy \"PostgresDifferential\"")
			}
		case v1.BackupStrategyPostgresDifferential:
			if n := item.EffectiveRetainFull(); n < v1.MinRetainFull || n > v1.MaxRetainFull {
				return handlerutil.NewValidationError("spec.backup.items[].retainFull", n,
					fmt.Sprintf("backup item %s: retainFull must be between %d and %d", item.Volume, v1.MinRetainFull, v1.MaxRetainFull))
			}
			if err := validatePostgresDifferentialMount(spec, item.Volume, postgresImage); err != nil {
				return err
			}
		default:
			return handlerutil.NewValidationError("spec.backup.items[].strategy", item.Strategy,
				"backup item "+item.Volume+": strategy must be \"Offline\" or \"PostgresDifferential\"")
		}
	}

	if spec.HasPostgresDifferential() {
		for _, vol := range spec.Volumes {
			if vol.Type == v1.VolumeTypeManaged && spec.BackupStrategyFor(vol.Name) != v1.BackupStrategyPostgresDifferential {
				return handlerutil.NewValidationError("spec.backup.items", vol.Name,
					"Managed volume "+vol.Name+" would be backed up offline, but a Project using \"PostgresDifferential\" "+
						"cannot mix strategies yet; give every Managed volume strategy \"PostgresDifferential\"")
			}
		}
	}
	return nil
}

// validatePostgresDifferentialMount requires volume to be mounted by exactly one
// service, running PostgresDifferentialImage, at PostgresDifferentialDataPath.
func validatePostgresDifferentialMount(spec v1.ProjectSpec, volume, postgresImage string) error {
	var mounts []string
	for _, svc := range spec.Services {
		for _, vm := range svc.VolumeMounts {
			if vm.Name != volume {
				continue
			}
			mounts = append(mounts, svc.Name)
			if svc.Image != postgresImage {
				return handlerutil.NewValidationError("spec.services[].image", svc.Image,
					fmt.Sprintf("service %s mounts PostgresDifferential volume %s, so its image must be %q",
						svc.Name, volume, postgresImage))
			}
			if vm.MountPath != v1.PostgresDifferentialDataPath {
				return handlerutil.NewValidationError("spec.services[].volumeMounts[].mountPath", vm.MountPath,
					fmt.Sprintf("service %s mounts PostgresDifferential volume %s, so it must be mounted at %q",
						svc.Name, volume, v1.PostgresDifferentialDataPath))
			}
			if err := validatePostgresDifferentialEnv(svc, volume); err != nil {
				return err
			}
		}
	}
	switch len(mounts) {
	case 1:
		return nil
	case 0:
		return handlerutil.NewValidationError("spec.backup.items[].volume", volume,
			"PostgresDifferential volume "+volume+" is not mounted by any service")
	default:
		return handlerutil.NewValidationError("spec.backup.items[].volume", volume,
			fmt.Sprintf("PostgresDifferential volume %s must be mounted by exactly one service, found %d (%s)",
				volume, len(mounts), strings.Join(mounts, ", ")))
	}
}

// validatePostgresDifferentialEnv rejects environment variables that would put
// PostgreSQL's data or WAL somewhere other than the mounted volume. The
// image's entrypoint honours all three; with any of them, the database would
// live partly outside the volume that provenance, placement and backup treat
// as its data, and the part outside is lost when the container is recreated.
//
// A value from a Secret is refused too: what it holds is only known when the
// agent resolves it, after the Project has been accepted.
func validatePostgresDifferentialEnv(svc v1.ServiceDef, volume string) error {
	for _, env := range svc.Env {
		switch env.Name {
		case "PGDATA", "POSTGRES_INITDB_WALDIR", "POSTGRES_INITDB_ARGS":
		default:
			continue
		}
		if env.ValueFrom != nil {
			return handlerutil.NewValidationError("spec.services[].env[].valueFrom", env.Name,
				fmt.Sprintf("service %s uses PostgresDifferential volume %s, so %s must be a literal value that can be checked when the Project is applied",
					svc.Name, volume, env.Name))
		}
		switch env.Name {
		case "PGDATA":
			if env.Value != v1.PostgresDifferentialDataPath {
				return handlerutil.NewValidationError("spec.services[].env[].value", env.Value,
					fmt.Sprintf("service %s uses PostgresDifferential volume %s, so PGDATA must be %q or unset",
						svc.Name, volume, v1.PostgresDifferentialDataPath))
			}
		case "POSTGRES_INITDB_WALDIR":
			return handlerutil.NewValidationError("spec.services[].env[].name", env.Name,
				fmt.Sprintf("service %s uses PostgresDifferential volume %s, so the WAL directory cannot be moved out of it",
					svc.Name, volume))
		case "POSTGRES_INITDB_ARGS":
			for _, arg := range strings.Fields(env.Value) {
				if !allowedInitdbArg(arg) {
					return handlerutil.NewValidationError("spec.services[].env[].value", env.Value,
						fmt.Sprintf("service %s uses PostgresDifferential volume %s, so POSTGRES_INITDB_ARGS may only contain "+
							"-k, --data-checksums or --NAME=VALUE for a supported setting, written out in full (%q is not)",
							svc.Name, volume, arg))
				}
			}
		}
	}
	return nil
}

// initdbSettings are the initdb options POSTGRES_INITDB_ARGS may set, all of
// which take a value and none of which moves data out of PGDATA. An allowlist,
// not a check for the dangerous ones: initdb accepts any unambiguous prefix of
// a long option, so "--wald=/tmp/x" moves the WAL as surely as "--waldir"
// does, and no list of forbidden spellings can be complete.
var initdbSettings = map[string]bool{
	"--encoding":        true,
	"--locale":          true,
	"--lc-collate":      true,
	"--lc-ctype":        true,
	"--lc-messages":     true,
	"--lc-monetary":     true,
	"--lc-numeric":      true,
	"--lc-time":         true,
	"--locale-provider": true,
	"--icu-locale":      true,
	"--auth":            true,
	"--auth-host":       true,
	"--auth-local":      true,
	"--wal-segsize":     true,
}

// initdbValue is what a setting's value may contain. The image's entrypoint
// passes POSTGRES_INITDB_ARGS through eval, so a value is shell input: "$",
// quotes, backslashes and the like would let it expand into further arguments
// (--encoding=UTF8${IFS}--waldir=/tmp/x is two of them). Every encoding,
// locale, authentication method and segment size is spelled with these.
var initdbValue = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)

// allowedInitdbArg reports whether arg is a flag initdb may be given for a
// PostgresDifferential volume: -k or --data-checksums, or a setting from
// initdbSettings written as --NAME=VALUE with a literal value. Anything else,
// including a value given as a separate word, is refused rather than parsed.
func allowedInitdbArg(arg string) bool {
	if arg == "-k" || arg == "--data-checksums" {
		return true
	}
	name, value, ok := strings.Cut(arg, "=")
	return ok && initdbSettings[name] && initdbValue.MatchString(value)
}

// validateEnvVar enforces the EnvVar contract from CARA-57: value and
// valueFrom are mutually exclusive, and a secretKeyRef must name both the
// Secret and the key. Whether the referenced Secret actually exists is NOT
// checked here — that is resolved (and failed) by the agent at reconcile
// time, since the Secret can legitimately be created after the Project.
func validateEnvVar(svcName string, env v1.EnvVar) error {
	if env.Name == "" {
		return handlerutil.NewValidationError("spec.services[].env[].name", nil,
			"service "+svcName+": each env var must have a non-empty name")
	}
	if env.ValueFrom == nil {
		return nil
	}
	if env.Value != "" {
		return handlerutil.NewValidationError("spec.services[].env[]", env.Name,
			"service "+svcName+", env "+env.Name+": value and valueFrom are mutually exclusive")
	}
	ref := env.ValueFrom.SecretKeyRef
	if ref == nil {
		return handlerutil.NewValidationError("spec.services[].env[].valueFrom", env.Name,
			"service "+svcName+", env "+env.Name+": valueFrom requires secretKeyRef")
	}
	if ref.Name == "" || ref.Key == "" {
		return handlerutil.NewValidationError("spec.services[].env[].valueFrom.secretKeyRef", env.Name,
			"service "+svcName+", env "+env.Name+": secretKeyRef.name and secretKeyRef.key must both be non-empty")
	}
	return nil
}

func (h *Handler) createProject(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "createProject")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	var project v1.Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	if project.Name == "" {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", nil, "metadata.name is required"), logger)
		return
	}

	if err := v1.ValidateName(project.Name); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", project.Name, err.Error()), logger)
		return
	}

	if err := v1.ValidateNamespace(project.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", project.Namespace, err.Error()), logger)
		return
	}
	if project.Namespace == "" {
		project.Namespace = v1.DefaultNamespace
	}

	if err := validateProjectSpecFor(project.Spec, h.postgresImage); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Status is server-owned, all of it, so whatever the client sent is
	// replaced rather than filtered field by field. A filter only covers the
	// fields someone remembered to list: a client could otherwise create a
	// Project already Running on a Node of its choosing, or with forged
	// recovery evidence, and controllers would act on it as fact.
	//
	// A freshly created Project is also the only thing that may claim
	// NeverAssigned at generation zero: the server is watching it from birth,
	// so it can prove no Agent has ever owned it. This is what later lets a
	// still-Pending Project be hard-deleted directly.
	project.Status = v1.InitialProjectStatus()

	// A Project asking for a strategy that does not run yet must say so from
	// the moment it exists; otherwise an accepted manifest reads as protected
	// data. Server-owned, so any value a client sent is replaced.
	project.Status.Conditions = v1.RemoveConditions(project.Status.Conditions, v1.ConditionTypeBackupActive)
	if project.Spec.HasPostgresDifferential() {
		project.Status.Conditions = append(project.Status.Conditions, v1.Condition{
			Type:               v1.ConditionTypeBackupActive,
			Status:             v1.ConditionFalse,
			LastTransitionTime: time.Now().UTC(),
			Reason:             v1.ReasonStrategyNotActive,
			Message:            "PostgresDifferential backups are not running yet; nothing backs up this Project's data",
		})
	}

	if err := h.store.CreateProject(traceCtx, &project); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project already exists: %s: %w", project.Name, store.ErrAlreadyExists), logger)
			return
		}
		logger.Error("CreateProject failed", zap.String("name", project.Name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	project.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Project"}
	handlerutil.WriteJSONResponse(w, http.StatusCreated, &project)
}

func (h *Handler) updateProject(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "updateProject")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	var project v1.Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	// Reject requests where the body name does not match the URL path.
	if project.Name != "" && project.Name != name {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", project.Name,
				fmt.Sprintf("metadata.name %q does not match URL path %q", project.Name, name)), logger)
		return
	}
	project.Name = name

	if err := v1.ValidateNamespace(project.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", project.Namespace, err.Error()), logger)
		return
	}
	if project.Namespace == "" {
		project.Namespace = v1.DefaultNamespace
	}

	if err := validateProjectSpecFor(project.Spec, h.postgresImage); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Checked here for a clear field-level answer; the store repeats the check
	// inside its update, which is what holds against a concurrent update.
	if current, err := h.store.GetProject(traceCtx, name); err == nil && !v1.SameBackupItems(current.Spec, project.Spec) {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("spec.backup.items", nil,
				"spec.backup.items cannot change after the Project is created; in-place changes of backup strategy or retainFull are not supported"),
			logger)
		return
	}

	// TODO: add metadata.resourceVersion / optimistic concurrency in a future PR.

	if err := h.store.UpdateProjectSpec(traceCtx, &project); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		if errors.Is(err, store.ErrConflictState) {
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
		logger.Error("UpdateProjectSpec failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Fetch the updated resource to return the full object (including status).
	updated, err := h.store.GetProject(traceCtx, name)
	if err != nil {
		logger.Error("GetProject failed after update", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, updated)
}

// listProjects handles GET /api/v1/projects.
//
// Optional query parameters:
//
//	?phase=Scheduled            — filter by a single status.phase
//	?phase=Scheduled&phase=Running — filter by multiple phases (OR)
//	?nodeRef=worker-1           — filter by status.nodeRef
//
// Phase and nodeRef filters are ANDed when both are present. Multiple ?phase=
// values are ORed. Because the store nodeRef filtering is applied in-process
// after the store call, this is acceptable for MVP scale.
func (h *Handler) listProjects(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "listProjects")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	phaseValues := r.URL.Query()["phase"]
	nodeRefFilter := r.URL.Query().Get("nodeRef")

	var (
		projects []*v1.Project
		err      error
	)

	switch len(phaseValues) {
	case 0:
		projects, err = h.store.ListProjects(traceCtx)
	case 1:
		projects, err = h.store.ListProjectsByPhase(traceCtx, v1.ProjectPhase(phaseValues[0]))
	default:
		phases := make([]v1.ProjectPhase, len(phaseValues))
		for i, p := range phaseValues {
			phases[i] = v1.ProjectPhase(p)
		}
		projects, err = h.store.ListProjectsByPhases(traceCtx, phases)
	}
	if err != nil {
		logger.Error("ListProjects failed", zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Apply nodeRef filter in-process.
	if nodeRefFilter != "" {
		filtered := projects[:0]
		for _, p := range projects {
			if p.Status.NodeRef == nodeRefFilter {
				filtered = append(filtered, p)
			}
		}
		projects = filtered
	}

	list := v1.ProjectList{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "ProjectList"},
		Items:    make([]v1.Project, len(projects)),
	}
	for i, p := range projects {
		list.Items[i] = *p
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, list)
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "getProject")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	project, err := h.store.GetProject(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetProject failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, project)
}

// deleteProject handles DELETE /api/v1/projects/{name}.
//
// Deletion is a two-phase process modelled after Kubernetes finalizers:
//
//  1. Projects that have never been scheduled (Pending or Failed) hold no
//     Docker resources, so they are removed from the store immediately and
//     the caller receives 204 No Content.
//
//  2. Projects that are Scheduled, Running, or already Terminating have live
//     (or potentially live) Docker resources on a Node.  The handler
//     transitions them to Terminating and returns 202 Accepted.  The Agent
//     polls for Terminating projects, tears down all Docker resources, then
//     reports Terminated.  The ProjectTerminationController observes that
//     phase and performs the final store deletion.
//
// Calling DELETE on an already-Terminating project is idempotent (202).
//
// When the ?force=true query parameter is set, the handler bypasses the
// two-phase lifecycle and directly hard-deletes the project from the store
// regardless of current phase, returning 204 No Content.  A warning is logged
// when the project has a nodeRef set, indicating Docker resources on the node
// may be orphaned.
func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "deleteProject")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	force := r.URL.Query().Get("force") == "true"

	project, err := h.store.GetProject(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetProject failed during delete", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Force-delete: bypass the two-phase lifecycle and hard-delete immediately.
	if force {
		if project.Status.NodeRef != "" {
			logger.Warn("Force-deleting project with nodeRef set; Docker resources on the node may be orphaned",
				zap.String("project", name),
				zap.String("phase", string(project.Status.Phase)),
				zap.String("nodeRef", project.Status.NodeRef))
		}
		if err := h.store.DeleteProject(traceCtx, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
				return
			}
			logger.Error("Force DeleteProject failed", zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
		logger.Info("Project force-deleted", zap.String("project", name),
			zap.String("phase", string(project.Status.Phase)))
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Direct hard-delete is only safe when the Project was provably never
	// assigned to a Node. NeverAssigned history means no Agent ever held runtime
	// for it, so there is nothing to strand. A Pending or Failed Project with
	// Known or Unknown history may still have containers or Managed volume data
	// on some Node — an empty nodeRef or a zero generation is not proof
	// otherwise — so it must enter the Terminating cleanup lifecycle, where the
	// Agent stops and removes resources before the record is dropped.
	// WorkloadStopped is never a substitute for ResourcesRemoved.
	//
	// The read above is only a guess. The Scheduler may commit the first
	// assignment between it and the delete, so the store checks the same
	// predicate again on the row it locks for the delete. If it no longer
	// holds, the request is decided again from the current state — normally
	// that means entering Terminating like any assigned Project — and never
	// forced through.
	if v1.ImmediateDeletionBlocker(project) == "" {
		err := h.store.FinalizeProject(traceCtx, name, project.ObjectMeta.UID, v1.ImmediateDeletionBlocker)
		switch {
		case err == nil:
			logger.Info("Project deleted immediately", zap.String("project", name),
				zap.String("phase", string(project.Status.Phase)),
				zap.String("assignmentHistory", string(project.Status.AssignmentHistory)))
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(err, store.ErrNotFound):
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		case errors.Is(err, store.ErrStaleAssignment):
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project %s was deleted and recreated during this request; retry to delete the new one: %w",
					name, store.ErrConflictState), logger)
			return
		case errors.Is(err, store.ErrConflictState):
			logger.Info("Project changed before it could be deleted immediately; deciding again",
				zap.String("project", name), zap.Error(err))
			current, getErr := h.store.GetProject(traceCtx, name)
			switch {
			case errors.Is(getErr, store.ErrNotFound):
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
				return
			case getErr != nil:
				logger.Error("GetProject failed during delete", zap.String("name", name), zap.Error(getErr))
				h.problemWriter.WriteError(traceCtx, w, getErr, logger)
				return
			case current.ObjectMeta.UID != project.ObjectMeta.UID:
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project %s was deleted and recreated during this request; retry to delete the new one: %w",
						name, store.ErrConflictState), logger)
				return
			}
			project = current
		default:
			logger.Error("FinalizeProject failed", zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
	}

	switch project.Status.Phase {
	case v1.ProjectPhaseTerminating, v1.ProjectPhaseTerminated:
		// Already in progress; idempotent. Terminated is answered the same
		// way: its resources are confirmed gone and the record is about to be
		// finalized, so moving it back to Terminating would only reopen a
		// deletion that has finished.
		logger.Info("Project already terminating", zap.String("project", name),
			zap.String("phase", string(project.Status.Phase)))
		w.WriteHeader(http.StatusAccepted)

	default:
		// Scheduled, Running, or any future active phase: transition to Terminating.
		//
		// The phase is set on whatever status the store read, not on the copy
		// fetched above: a controller may have written NodeRef or a condition
		// in between, and replacing the whole status would discard it. The
		// write is pinned to the UID fetched above, so the Project the caller
		// asked to delete is the one that terminates, never a same-name
		// successor created in between.
		now := time.Now().UTC()
		uid := project.ObjectMeta.UID
		err := h.store.UpdateProjectStatusForUID(traceCtx, name, uid, func(status *v1.ProjectStatus) error {
			if status.Phase == v1.ProjectPhaseTerminating || status.Phase == v1.ProjectPhaseTerminated {
				// Reached Terminating or beyond since the read above.
				return nil
			}
			if !v1.PhaseTransitionAllowed(v1.WriterDeleteAPI, status.Phase, v1.ProjectPhaseTerminating) {
				return fmt.Errorf("%w: project %q is %s; it cannot enter Terminating from there",
					store.ErrConflictState, name, status.Phase)
			}
			status.Phase = v1.ProjectPhaseTerminating
			// The assigned Node now owes the removal of everything it holds
			// for this assignment. Recorded in the same write as the phase,
			// so the obligation list stays the complete set of Nodes that
			// may hold resources. A Project with no nodeRef has no current
			// assignment; any earlier Node is already in the list from its
			// release.
			if status.NodeRef != "" {
				status.EnsureRecovery().AddObligation(v1.AssignmentIdentity{
					ProjectUID:           uid,
					NodeName:             status.NodeRef,
					AssignmentGeneration: status.AssignmentGeneration,
				}, now, v1.ReleaseByDeletion)
			}
			return nil
		})
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
				return
			}
			if errors.Is(err, store.ErrStaleAssignment) {
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project %s was deleted and recreated during this request; retry to delete the new one: %w",
						name, store.ErrConflictState), logger)
				return
			}
			if errors.Is(err, store.ErrConflictState) {
				h.problemWriter.WriteError(traceCtx, w, err, logger)
				return
			}
			logger.Error("Marking project Terminating failed", zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
		logger.Info("Project marked as Terminating", zap.String("project", name),
			zap.String("previous_phase", string(project.Status.Phase)))
		w.WriteHeader(http.StatusAccepted)
	}
}

// statusPatchRequest is the body the Agent sends to report observed phase.
// Only Phase and an optional Message are accepted; other status fields
// (NodeRef, Conditions) are preserved from the existing record.
//
// UID, NodeRef, and AssignmentGeneration carry the assignment fence the Agent
// was granted (CARA-83). AssignmentGeneration is a pointer so a legacy Agent
// that omits it is distinguishable from one reporting generation zero: nil
// means "no fence sent", which is refused.
type statusPatchRequest struct {
	Phase                v1.ProjectPhase `json:"phase"`
	Reason               string          `json:"reason,omitempty"`
	Message              string          `json:"message,omitempty"`
	UID                  string          `json:"uid,omitempty"`
	NodeRef              string          `json:"nodeRef,omitempty"`
	AssignmentGeneration *int64          `json:"assignmentGeneration,omitempty"`
}

// resolveFence reads the assignment fields an Agent presented. An incomplete
// fence comes from an Agent that predates CARA-83 and is rejected as stale; the
// returned flag is then false, the problem is already written, and the caller
// must return.
func (h *Handler) resolveFence(
	ctx context.Context,
	w http.ResponseWriter,
	logger *zap.Logger,
	name, uid, nodeRef string,
	generation *int64,
) (store.AssignmentRef, bool) {
	if uid == "" || nodeRef == "" || generation == nil {
		h.problemWriter.WriteError(ctx, w,
			fmt.Errorf("%w: project %q: request is missing an assignment fence", store.ErrStaleAssignment, name), logger)
		return store.AssignmentRef{}, false
	}
	return store.AssignmentRef{UID: uid, NodeRef: nodeRef, Generation: *generation}, true
}

// patchStatus handles PATCH /api/v1/projects/{name}/status.
// The Agent calls this endpoint to transition a Scheduled Project to Running
// (or Failed).  Only the Agent should call this; the API server does not
// validate the caller identity in the MVP.
func (h *Handler) patchStatus(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "patchStatus")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	var req statusPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	if req.Phase == "" {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("phase", nil, "phase is required"), logger)
		return
	}

	if !req.Phase.IsValid() {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("phase", req.Phase,
				"invalid phase "+string(req.Phase)+": must be one of Pending, Scheduled, Running, Failed, Terminating, Terminated"), logger)
		return
	}

	ref, ok := h.resolveFence(traceCtx, w, logger, name, req.UID, req.NodeRef, req.AssignmentGeneration)
	if !ok {
		return
	}

	// Fixed before the closure: the closure may run more than once, and this
	// records when the agent observed the phase, not when a retry landed.
	observedAt := time.Now().UTC()

	// Only the fields this request carries are touched; everything else —
	// NodeRef, conditions owned by controllers — is whatever the store read on
	// this attempt. Replacing the whole status here is what used to discard a
	// controller's concurrent write.
	mutate := func(status *v1.ProjectStatus) error {
		// Checked on the snapshot this attempt will write, so a deletion that
		// lands between the Agent's observation and this write wins: a late
		// Running report cannot turn a Terminating Project back into a running
		// one, even though its fence still names the same assignment.
		if !v1.PhaseTransitionAllowed(v1.WriterAgent, status.Phase, req.Phase) {
			return fmt.Errorf("%w: project %q is %s; an Agent cannot report it %s",
				store.ErrConflictState, name, status.Phase, req.Phase)
		}
		// The phase may say the resources are gone only once the evidence that
		// they are is recorded, for the assignment making the report.
		if req.Phase == v1.ProjectPhaseTerminated && status.Phase != v1.ProjectPhaseTerminated &&
			!status.Recovery.ResourcesRemovedFor(status.NodeRef, status.AssignmentGeneration) {
			return fmt.Errorf("%w: project %q: node %s has not recorded ResourcesRemoved for generation %d",
				store.ErrConflictState, name, status.NodeRef, status.AssignmentGeneration)
		}

		transitioned := status.Phase != req.Phase
		status.Phase = req.Phase
		// A reported failure ends the healthy window in the same write. This
		// is the one recovery field an Agent report may change, and only to
		// clear it.
		if req.Phase == v1.ProjectPhaseFailed {
			status.Recovery.ClearHealthyWindow()
		}

		if req.Reason == "" && req.Message == "" {
			return nil
		}
		// UpsertCondition keeps the stored timestamp when this report says the
		// same thing as the last one, which is what makes an unchanged tick a
		// byte-level no-op the store can skip.
		status.Conditions = v1.UpsertCondition(status.Conditions, v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             req.Reason,
			Message:            req.Message,
			LastTransitionTime: observedAt,
		}, transitioned)
		return nil
	}

	// The fenced path re-asserts (UID, nodeRef, generation) inside the guarding
	// UPDATE, so a report from an Agent that has since lost ownership is
	// rejected as stale without touching the row.
	if err := h.store.ReportProjectStatusFenced(traceCtx, name, ref, mutate); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		if errors.Is(err, store.ErrConflictState) || errors.Is(err, store.ErrStaleAssignment) {
			// Expected whenever a report races a lifecycle change; the Agent
			// learns from the 409, and the server log need not.
			logger.Info("Project status patch refused", zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
		logger.Error("Project status patch failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Info("Project status patched",
		zap.String("project", name),
		zap.String("phase", string(req.Phase)),
	)
	w.WriteHeader(http.StatusNoContent)
}

// agentWritableConditions are the condition types an agent may write.
//
// Conditions are a shared namespace: Phase, NotReadyAt and TerminatingAt are
// owned by controllers and drive scheduling decisions. Letting an agent write
// those through this endpoint would let a buggy or compromised node forge the
// timers the rescheduler reads, so the endpoint accepts only the conditions
// agents legitimately own.
var agentWritableConditions = map[v1.ConditionType]bool{
	v1.ConditionTypeMaintenance:     true,
	v1.ConditionTypeRecoveryBlocked: true,
	v1.ConditionTypeTeardownBlocked: true,
}

// conditionPatchRequest is the body sent to
// PATCH /api/v1/projects/{name}/conditions/{type}. UID, NodeRef, and
// AssignmentGeneration carry the assignment fence, with the same
// legacy-vs-strict semantics as statusPatchRequest.
type conditionPatchRequest struct {
	Status               v1.ConditionStatus `json:"status"`
	Reason               string             `json:"reason,omitempty"`
	Message              string             `json:"message,omitempty"`
	UID                  string             `json:"uid,omitempty"`
	NodeRef              string             `json:"nodeRef,omitempty"`
	AssignmentGeneration *int64             `json:"assignmentGeneration,omitempty"`
}

// patchCondition handles PATCH /api/v1/projects/{name}/conditions/{type}.
//
// It merges exactly one condition without touching phase, which is what lets
// an agent advertise Maintenance during a backup while the Project stays
// Running. Reporting the backup by moving the phase instead would unlock the
// apply and delete paths that are deliberately closed for a Running Project.
func (h *Handler) patchCondition(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "patchCondition")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	condType := v1.ConditionType(r.PathValue("type"))

	if !agentWritableConditions[condType] {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("type", string(condType),
				fmt.Sprintf("condition type %q is not agent-writable", condType)), logger)
		return
	}

	var req conditionPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	switch req.Status {
	case v1.ConditionTrue, v1.ConditionFalse, v1.ConditionUnknown:
	default:
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("status", string(req.Status),
				`status must be "True", "False", or "Unknown"`), logger)
		return
	}

	// Unlike patchStatus, this stamps a fresh timestamp on every call rather
	// than going through v1.UpsertCondition, and that is deliberate for now.
	// PatchProjectCondition merges in SQL without reading first, so preserving
	// the stored timestamp would need either a read — losing the atomicity the
	// merge exists for — or the comparison pushed into jsonb.
	//
	// It is affordable because of who calls it, and each producer keeps it
	// that way. The backup runner writes Maintenance once when a backup starts
	// and clears it once when it ends, so every call really is a transition.
	// The recovery path evaluates RecoveryBlocked on every poll, but compares
	// against the condition it has just read and only writes when the reason
	// or message would change — so it too writes only on a transition.
	//
	// A producer that re-asserted the same condition on a schedule without
	// that comparison would turn each repeat into a write and a
	// project.updated event carrying no news, which is exactly what the guard
	// in patchStatus exists to prevent. Moving the comparison here, into the
	// merge, would cover every producer at once; it is not done yet because
	// it changes Maintenance too, whose staleness is measured from the
	// timestamp this stamps.
	ref, ok := h.resolveFence(traceCtx, w, logger, name, req.UID, req.NodeRef, req.AssignmentGeneration)
	if !ok {
		return
	}

	now := time.Now().UTC()
	condition := v1.Condition{
		Type:               condType,
		Status:             req.Status,
		Reason:             req.Reason,
		Message:            req.Message,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
	}

	if err := h.store.PatchProjectConditionFenced(traceCtx, name, ref, condition); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("PatchProjectCondition failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Info("Project condition patched",
		zap.String("project", name),
		zap.String("condition", string(condType)),
		zap.String("status", string(req.Status)),
	)
	w.WriteHeader(http.StatusNoContent)
}

// deleteCondition handles DELETE /api/v1/projects/{name}/conditions/{type}.
func (h *Handler) deleteCondition(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "deleteCondition")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	condType := v1.ConditionType(r.PathValue("type"))

	if !agentWritableConditions[condType] {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("type", string(condType),
				fmt.Sprintf("condition type %q is not agent-writable", condType)), logger)
		return
	}

	// DELETE carries no body, so the fence travels as query parameters. An
	// absent assignmentGeneration parameter is a nil generation — the same
	// "no fence sent" signal a missing body field is on the patch paths.
	uid := r.URL.Query().Get("uid")
	nodeRef := r.URL.Query().Get("nodeRef")
	var generation *int64
	if raw := r.URL.Query().Get("assignmentGeneration"); raw != "" {
		g, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("assignmentGeneration", raw,
					"assignmentGeneration must be an integer"), logger)
			return
		}
		generation = &g
	}

	ref, ok := h.resolveFence(traceCtx, w, logger, name, uid, nodeRef, generation)
	if !ok {
		return
	}

	if err := h.store.ClearProjectConditionFenced(traceCtx, name, ref, condType); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("ClearProjectCondition failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Info("Project condition cleared",
		zap.String("project", name),
		zap.String("condition", string(condType)),
	)
	w.WriteHeader(http.StatusNoContent)
}

// evidenceRequest is the body of the recovery evidence endpoints: the
// assignment fence and nothing else. The evidence identity is the fence; the
// server stamps the observation time.
type evidenceRequest struct {
	UID                  string `json:"uid"`
	NodeRef              string `json:"nodeRef"`
	AssignmentGeneration *int64 `json:"assignmentGeneration"`
}

// decodeEvidenceFence reads an evidenceRequest and requires a complete fence.
//
// Unlike the condition and status paths, a missing fence is refused in every
// mode: the evidence records which assignment it is about, and without a fence
// there is no assignment to record.
func (h *Handler) decodeEvidenceFence(ctx context.Context, w http.ResponseWriter, r *http.Request, logger *zap.Logger, name string) (store.AssignmentRef, bool) {
	var req evidenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return store.AssignmentRef{}, false
	}
	if req.UID == "" || req.NodeRef == "" || req.AssignmentGeneration == nil {
		h.problemWriter.WriteError(ctx, w,
			fmt.Errorf("%w: project %q: evidence requires a complete assignment fence", store.ErrStaleAssignment, name), logger)
		return store.AssignmentRef{}, false
	}
	return store.AssignmentRef{UID: req.UID, NodeRef: req.NodeRef, Generation: *req.AssignmentGeneration}, true
}

// writeEvidenceResult maps a store result for an evidence write to a response.
func (h *Handler) writeEvidenceResult(ctx context.Context, w http.ResponseWriter, logger *zap.Logger, name, kind string, err error) {
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(ctx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		if !errors.Is(err, store.ErrStaleAssignment) && !errors.Is(err, store.ErrConflictState) {
			logger.Error("Recording recovery evidence failed", zap.String("name", name), zap.String("evidence", kind), zap.Error(err))
		}
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Recovery evidence recorded", zap.String("project", name), zap.String("evidence", kind))
	w.WriteHeader(http.StatusNoContent)
}

// putWorkloadStopped handles PUT /api/v1/projects/{name}/recovery/workload-stopped.
//
// The currently assigned Agent calls it after confirming every Project
// container is stopped. The store checks the fence and phase atomically with
// the write; re-recording the same evidence is a no-op.
func (h *Handler) putWorkloadStopped(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "putWorkloadStopped")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)
	name := r.PathValue("name")

	ref, ok := h.decodeEvidenceFence(traceCtx, w, r, logger, name)
	if !ok {
		return
	}
	err := h.store.RecordWorkloadStopped(traceCtx, name, ref, time.Now().UTC())
	h.writeEvidenceResult(traceCtx, w, logger, name, "WorkloadStopped", err)
}

// putManagedDataQuarantined handles
// PUT /api/v1/projects/{name}/recovery/managed-data-quarantined.
//
// The Agent of a cleanup obligation calls it after its local Managed data has
// left the active path. It lets the Node hold the Project again and grants no
// other authority.
func (h *Handler) putManagedDataQuarantined(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "putManagedDataQuarantined")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)
	name := r.PathValue("name")

	ref, ok := h.decodeEvidenceFence(traceCtx, w, r, logger, name)
	if !ok {
		return
	}
	err := h.store.RecordManagedDataQuarantined(traceCtx, name, ref, time.Now().UTC())
	h.writeEvidenceResult(traceCtx, w, logger, name, "ManagedDataQuarantined", err)
}

// backupIDPattern is the shape backup.NewBackupID produces. The Agent builds
// object store keys from the committed value, so it is checked here too.
var backupIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{8}$`)

// committedBackupRequest is the body of the committed-backup endpoint.
type committedBackupRequest struct {
	UID                  string `json:"uid"`
	NodeRef              string `json:"nodeRef"`
	AssignmentGeneration *int64 `json:"assignmentGeneration"`
	BackupID             string `json:"backupID"`
}

// putCommittedBackup handles PUT /api/v1/projects/{name}/recovery/committed-backup.
//
// The currently assigned Agent calls it after a generation's archives and
// manifest are in the object store. Until then the generation is not a restore
// source, and an Agent whose assignment was released cannot make it one.
func (h *Handler) putCommittedBackup(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "putCommittedBackup")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)
	name := r.PathValue("name")

	var req committedBackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}
	if !backupIDPattern.MatchString(req.BackupID) {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("backupID", req.BackupID, "backupID is not a valid backup generation"), logger)
		return
	}
	ref, ok := h.resolveFence(traceCtx, w, logger, name, req.UID, req.NodeRef, req.AssignmentGeneration)
	if !ok {
		return
	}
	err := h.store.RecordCommittedBackup(traceCtx, name, ref, req.BackupID, time.Now().UTC())
	h.writeEvidenceResult(traceCtx, w, logger, name, "CommittedBackup", err)
}

// putResourcesRemoved handles PUT /api/v1/projects/{name}/recovery/resources-removed.
//
// The Agent of a cleanup obligation calls it after removing every
// deletion-scoped runtime resource it held for that obligation's assignment.
// It resolves that one obligation and grants no other authority.
func (h *Handler) putResourcesRemoved(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "putResourcesRemoved")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)
	name := r.PathValue("name")

	ref, ok := h.decodeEvidenceFence(traceCtx, w, r, logger, name)
	if !ok {
		return
	}
	err := h.store.RecordResourcesRemoved(traceCtx, name, ref, time.Now().UTC())
	h.writeEvidenceResult(traceCtx, w, logger, name, "ResourcesRemoved", err)
}
