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
package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
}

// NewHandler creates a Project Handler.
func NewHandler(logger *zap.Logger, s store.ProjectStore, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		store:         s,
		tracer:        otel.Tracer("project/handler"),
		problemWriter: pw,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/projects", mid.HandlerFunc(h.createProject))
	mux.HandleFunc("PUT /api/v1/projects/{name}", mid.HandlerFunc(h.updateProject))
	mux.HandleFunc("GET /api/v1/projects", mid.HandlerFunc(h.listProjects))
	mux.HandleFunc("GET /api/v1/projects/{name}", mid.HandlerFunc(h.getProject))
	mux.HandleFunc("DELETE /api/v1/projects/{name}", mid.HandlerFunc(h.deleteProject))
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", mid.HandlerFunc(h.patchStatus))
}

// ── handlers ──────────────────────────────────────────────────────────────────

// validateProjectSpec validates the project spec, returning a handlerutil.ValidationError
// if any rule is violated, or nil if the spec is valid.
func validateProjectSpec(spec v1.ProjectSpec) error {
	if len(spec.Services) == 0 {
		return handlerutil.NewValidationError("spec.services", nil, "spec.services must contain at least one service")
	}
	for _, svc := range spec.Services {
		if svc.Name == "" {
			return handlerutil.NewValidationError("spec.services[].name", nil, "each service must have a non-empty name")
		}
		if svc.Image == "" {
			return handlerutil.NewValidationError("spec.services[].image", svc.Name, "service "+svc.Name+": image is required")
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

	if err := validateProjectSpec(project.Spec); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// New projects start in Pending phase; the Scheduler will assign a node.
	if project.Status.Phase == "" {
		project.Status.Phase = v1.ProjectPhasePending
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

	if err := validateProjectSpec(project.Spec); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
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

	switch project.Status.Phase {
	case v1.ProjectPhasePending, v1.ProjectPhaseFailed:
		// No Docker resources exist; safe to delete immediately.
		if err := h.store.DeleteProject(traceCtx, name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
				return
			}
			logger.Error("DeleteProject failed", zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w, err, logger)
			return
		}
		logger.Info("Project deleted immediately", zap.String("project", name),
			zap.String("phase", string(project.Status.Phase)))
		w.WriteHeader(http.StatusNoContent)

	case v1.ProjectPhaseTerminating:
		// Already in progress; idempotent.
		logger.Info("Project already terminating", zap.String("project", name))
		w.WriteHeader(http.StatusAccepted)

	default:
		// Scheduled, Running, or any future active phase: transition to Terminating.
		status := project.Status
		status.Phase = v1.ProjectPhaseTerminating
		if err := h.store.UpdateProjectStatus(traceCtx, name, status); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.problemWriter.WriteError(traceCtx, w,
					fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
				return
			}
			logger.Error("UpdateProjectStatus (Terminating) failed", zap.String("name", name), zap.Error(err))
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
type statusPatchRequest struct {
	Phase   v1.ProjectPhase `json:"phase"`
	Reason  string          `json:"reason,omitempty"`
	Message string          `json:"message,omitempty"`
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

	// Fetch existing status to preserve fields we do not overwrite (nodeRef, etc.).
	existing, err := h.store.GetProject(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetProject failed during status patch", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	status := existing.Status
	status.Phase = req.Phase

	// Update or append a Phase condition when reason/message are provided.
	if req.Reason != "" || req.Message != "" {
		now := time.Now().UTC()
		cond := v1.Condition{
			Type:               v1.ConditionTypePhase,
			Status:             v1.ConditionTrue,
			Reason:             req.Reason,
			Message:            req.Message,
			LastTransitionTime: now,
		}
		updated := false
		for i, c := range status.Conditions {
			if c.Type == v1.ConditionTypePhase {
				status.Conditions[i] = cond
				updated = true
				break
			}
		}
		if !updated {
			status.Conditions = append(status.Conditions, cond)
		}
	}

	if err := h.store.UpdateProjectStatus(traceCtx, name, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("project not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("UpdateProjectStatus failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Info("Project status patched",
		zap.String("project", name),
		zap.String("phase", string(req.Phase)),
	)
	w.WriteHeader(http.StatusNoContent)
}
