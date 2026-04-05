// Package node provides the HTTP handler for Node resources.
//
// Routes registered:
//
//	POST   /api/v1/nodes                   — register / create a Node
//	GET    /api/v1/nodes                   — list all Nodes
//	GET    /api/v1/nodes/{name}            — get a single Node
//	DELETE /api/v1/nodes/{name}            — delete a Node
//	POST   /api/v1/nodes/{name}/heartbeat  — Agent heartbeat (updates status only)
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"go.uber.org/zap"
)

// Handler implements apiserver.RouteRegistrar for Node endpoints.
type Handler struct {
	logger       *zap.Logger
	store        store.NodeStore
	projectStore ProjectLister
}

// ProjectLister is the narrow interface the node handler needs to check
// whether any projects are assigned to a node before allowing deletion.
type ProjectLister interface {
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*v1.Project, error)
}

// NewHandler creates a Node Handler.
func NewHandler(logger *zap.Logger, s store.NodeStore, ps ProjectLister) *Handler {
	return &Handler{logger: logger, store: s, projectStore: ps}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/nodes", mid.HandlerFunc(h.createNode))
	mux.HandleFunc("GET /api/v1/nodes", mid.HandlerFunc(h.listNodes))
	mux.HandleFunc("GET /api/v1/nodes/{name}", mid.HandlerFunc(h.getNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{name}", mid.HandlerFunc(h.deleteNode))
	mux.HandleFunc("POST /api/v1/nodes/{name}/heartbeat", mid.HandlerFunc(h.heartbeat))
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (h *Handler) createNode(w http.ResponseWriter, r *http.Request) {
	var node v1.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if node.Name == "" {
		h.writeError(w, http.StatusBadRequest, "metadata.name is required")
		return
	}

	if err := v1.ValidateName(node.Name); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Initialise status to NotReady on creation; the Agent will push heartbeats
	// to transition it to Ready once the connection is confirmed.
	if node.Status.State == "" {
		node.Status.State = v1.NodeStateNotReady
	}

	if err := h.store.CreateNode(r.Context(), &node); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			h.writeError(w, http.StatusConflict, "node already exists: "+node.Name)
			return
		}
		h.logger.Error("CreateNode failed", zap.String("name", node.Name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	node.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"}
	h.writeJSON(w, http.StatusCreated, &node)
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := h.store.ListNodes(r.Context())
	if err != nil {
		h.logger.Error("ListNodes failed", zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	list := v1.NodeList{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "NodeList"},
		Items:    make([]v1.Node, len(nodes)),
	}
	for i, n := range nodes {
		list.Items[i] = *n
	}
	h.writeJSON(w, http.StatusOK, list)
}

func (h *Handler) getNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	node, err := h.store.GetNode(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "node not found: "+name)
			return
		}
		h.logger.Error("GetNode failed", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	h.writeJSON(w, http.StatusOK, node)
}

func (h *Handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Guard: reject deletion when projects are still assigned to this node.
	activePhases := []v1.ProjectPhase{
		v1.ProjectPhaseScheduled,
		v1.ProjectPhaseRunning,
		v1.ProjectPhaseTerminating,
	}
	projects, err := h.projectStore.ListProjectsByNodeRef(r.Context(), name, activePhases)
	if err != nil {
		h.logger.Error("ListProjectsByNodeRef failed during node delete",
			zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	if len(projects) > 0 {
		h.writeError(w, http.StatusConflict,
			fmt.Sprintf("cannot delete node %q: %d project(s) still assigned", name, len(projects)))
		return
	}

	if err := h.store.DeleteNode(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "node not found: "+name)
			return
		}
		h.logger.Error("DeleteNode failed", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// heartbeatRequest is the body the Agent sends on each heartbeat.
// All fields are optional; missing fields leave the corresponding status
// sub-fields unchanged (merged into the existing status row).
type heartbeatRequest struct {
	State       v1.NodeState         `json:"state,omitempty"`
	Network     v1.NodeNetworkStatus `json:"network,omitempty"`
	Capacity    v1.ResourceList      `json:"capacity,omitempty"`
	Allocatable v1.ResourceList      `json:"allocatable,omitempty"`
}

func (h *Handler) heartbeat(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	// Fetch existing status so we can merge rather than overwrite.
	existing, err := h.store.GetNode(r.Context(), name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "node not found: "+name)
			return
		}
		h.logger.Error("GetNode failed during heartbeat", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	var req heartbeatRequest
	// Body is optional; an empty / missing body is a pure timestamp update.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
			return
		}
	}

	// Validate state before persisting.
	if req.State != "" && !req.State.IsValid() {
		h.writeError(w, http.StatusBadRequest,
			"invalid state "+string(req.State)+": must be one of Ready, NotReady, Draining")
		return
	}

	// Merge incoming fields into existing status.
	status := existing.Status
	status.LastHeartbeat = time.Now().UTC()
	if req.State != "" {
		status.State = req.State
	}
	if req.Network != (v1.NodeNetworkStatus{}) {
		status.Network = req.Network
	}
	if len(req.Capacity) > 0 {
		status.Capacity = req.Capacity
	}
	if len(req.Allocatable) > 0 {
		status.Allocatable = req.Allocatable
	}

	if err := h.store.UpdateNodeStatus(r.Context(), name, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, "node not found: "+name)
			return
		}
		h.logger.Error("UpdateNodeStatus failed", zap.String("name", name), zap.Error(err))
		h.writeError(w, http.StatusInternalServerError, "internal server error")
		return
	}

	h.logger.Debug("Heartbeat received", zap.String("node", name))
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
