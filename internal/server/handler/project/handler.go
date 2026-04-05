// Package project provides the HTTP handler for Project resources.
//
// Routes registered:
//
//	POST   /api/v1/projects                  — create a Project
//	GET    /api/v1/projects                  — list Projects (supports ?phase= and ?nodeRef= filters)
//	GET    /api/v1/projects/{name}           — get a single Project
//	DELETE /api/v1/projects/{name}           — delete a Project
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
	mux.HandleFunc("GET /api/v1/projects", mid.HandlerFunc(h.listProjects))
	mux.HandleFunc("GET /api/v1/projects/{name}", mid.HandlerFunc(h.getProject))
	mux.HandleFunc("DELETE /api/v1/projects/{name}", mid.HandlerFunc(h.deleteProject))
	mux.HandleFunc("PATCH /api/v1/projects/{name}/status", mid.HandlerFunc(h.patchStatus))
}

// ── handlers ──────────────────────────────────────────────────────────────────

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

	// Validate spec: at least one service with a non-empty image is required.
	if len(project.Spec.Services) == 0 {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("spec.services", nil, "spec.services must contain at least one service"), logger)
		return
	}
	for _, svc := range project.Spec.Services {
		if svc.Name == "" {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("spec.services[].name", nil, "each service must have a non-empty name"), logger)
			return
		}
		if svc.Image == "" {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("spec.services[].image", svc.Name, "service "+svc.Name+": image is required"), logger)
			return
		}
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
func (h *Handler) deleteProject(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "deleteProject")
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
		logger.Error("GetProject failed during delete", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
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
