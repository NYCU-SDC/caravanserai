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
	"net/http"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"go.uber.org/zap"
)

// Handler implements apiserver.RouteRegistrar for Project endpoints.
type Handler struct {
	logger *zap.Logger
	store  store.ProjectStore
}

// NewHandler creates a Project Handler.
func NewHandler(logger *zap.Logger, s store.ProjectStore) *Handler {
	return &Handler{logger: logger, store: s}
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
	var project v1.Project
	if err := json.NewDecoder(r.Body).Decode(&project); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if project.Name == "" {
		h.writeError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	// New projects start in Pending phase; the Scheduler will assign a node.
	if project.Status.Phase == "" {
		project.Status.Phase = v1.ProjectPhasePending
	}

	if err := h.store.CreateProject(r.Context(), &project); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			h.writeError(w, http.StatusConflict, "project already exists: "+project.Name)
			return
		}
		h.logger.Error("CreateProject failed", zap.String("name", project.Name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	h.writeJSON(w, http.StatusCreated, &project)
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
	phaseValues := r.URL.Query()["phase"]
	nodeRefFilter := r.URL.Query().Get("nodeRef")

	var (
		projects []*v1.Project
		err      error
	)

	switch len(phaseValues) {
	case 0:
		projects, err = h.store.ListProjects(r.Context())
	case 1:
		projects, err = h.store.ListProjectsByPhase(r.Context(), v1.ProjectPhase(phaseValues[0]))
	default:
		phases := make([]v1.ProjectPhase, len(phaseValues))
		for i, p := range phaseValues {
			phases[i] = v1.ProjectPhase(p)
		}
		projects, err = h.store.ListProjectsByPhases(r.Context(), phases)
	}
	if err != nil {
		h.logger.Error("ListProjects failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
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
	h.writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getProject(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	project, err := h.store.GetProject(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "project not found: "+name)
			return
		}
		h.logger.Error("GetProject failed", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	h.writeJSON(w, http.StatusOK, project)
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
	name := r.PathValue("name")

	project, err := h.store.GetProject(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "project not found: "+name)
			return
		}
		h.logger.Error("GetProject failed during delete", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	switch project.Status.Phase {
	case v1.ProjectPhasePending, v1.ProjectPhaseFailed:
		// No Docker resources exist; safe to delete immediately.
		if err := h.store.DeleteProject(r.Context(), name); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.writeError(w, http.StatusNotFound, "project not found: "+name)
				return
			}
			h.logger.Error("DeleteProject failed", zap.String("name", name), zap.Error(err))
			h.writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		h.logger.Info("Project deleted immediately", zap.String("project", name),
			zap.String("phase", string(project.Status.Phase)))
		w.WriteHeader(http.StatusNoContent)

	case v1.ProjectPhaseTerminating:
		// Already in progress; idempotent.
		h.logger.Info("Project already terminating", zap.String("project", name))
		w.WriteHeader(http.StatusAccepted)

	default:
		// Scheduled, Running, or any future active phase: transition to Terminating.
		status := project.Status
		status.Phase = v1.ProjectPhaseTerminating
		if err := h.store.UpdateProjectStatus(r.Context(), name, status); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				h.writeError(w, http.StatusNotFound, "project not found: "+name)
				return
			}
			h.logger.Error("UpdateProjectStatus (Terminating) failed", zap.String("name", name), zap.Error(err))
			h.writeError(w, http.StatusInternalServerError, "internal server error")
			return
		}
		h.logger.Info("Project marked as Terminating", zap.String("project", name),
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
	name := r.PathValue("name")

	var req statusPatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.Phase == "" {
		h.writeError(w, http.StatusBadRequest, "phase is required")
		return
	}

	// Fetch existing status to preserve fields we do not overwrite (nodeRef, etc.).
	existing, err := h.store.GetProject(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "project not found: "+name)
			return
		}
		h.logger.Error("GetProject failed during status patch", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	status := existing.Status
	status.Phase = req.Phase

	// Update or append a Phase condition when reason/message are provided.
	if req.Reason != "" || req.Message != "" {
		condType := "Phase"
		now := time.Now().UTC()
		cond := v1.Condition{
			Type:               condType,
			Status:             v1.ConditionTrue,
			Reason:             req.Reason,
			Message:            req.Message,
			LastTransitionTime: now,
		}
		updated := false
		for i, c := range status.Conditions {
			if c.Type == condType {
				status.Conditions[i] = cond
				updated = true
				break
			}
		}
		if !updated {
			status.Conditions = append(status.Conditions, cond)
		}
	}

	if err := h.store.UpdateProjectStatus(r.Context(), name, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "project not found: "+name)
			return
		}
		h.logger.Error("UpdateProjectStatus failed", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	h.logger.Info("Project status patched",
		zap.String("project", name),
		zap.String("phase", string(req.Phase)),
	)
	w.WriteHeader(http.StatusNoContent)
}

// ── helpers ───────────────────────────────────────────────────────────────────

type errorResponse struct {
	Error string `json:"error"`
}

func (h *Handler) writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("Failed to encode JSON response", zap.Error(err))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, code int, msg string) {
	h.writeJSON(w, code, errorResponse{Error: strings.TrimSpace(msg)})
}
