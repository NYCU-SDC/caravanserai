// Package node provides the HTTP handler for Node resources.
//
// Routes registered:
//
//	POST   /api/v1/nodes                   — register / create a Node
//	PUT    /api/v1/nodes/{name}            — update a Node's spec
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

// Handler implements apiserver.RouteRegistrar for Node endpoints.
type Handler struct {
	logger        *zap.Logger
	store         store.NodeStore
	projectStore  ProjectLister
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter
}

// ProjectLister is the narrow interface the node handler needs to check
// whether any projects are assigned to a node before allowing deletion.
type ProjectLister interface {
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*v1.Project, error)
}

// NewHandler creates a Node Handler.
func NewHandler(logger *zap.Logger, s store.NodeStore, ps ProjectLister, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		store:         s,
		projectStore:  ps,
		tracer:        otel.Tracer("node/handler"),
		problemWriter: pw,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/nodes", mid.HandlerFunc(h.createNode))
	mux.HandleFunc("PUT /api/v1/nodes/{name}", mid.HandlerFunc(h.updateNode))
	mux.HandleFunc("GET /api/v1/nodes", mid.HandlerFunc(h.listNodes))
	mux.HandleFunc("GET /api/v1/nodes/{name}", mid.HandlerFunc(h.getNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{name}", mid.HandlerFunc(h.deleteNode))
	mux.HandleFunc("POST /api/v1/nodes/{name}/heartbeat", mid.HandlerFunc(h.heartbeat))
}

// ── handlers ──────────────────────────────────────────────────────────────────

func (h *Handler) createNode(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "createNode")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	var node v1.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	if node.Name == "" {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", nil, "metadata.name is required"), logger)
		return
	}

	if err := v1.ValidateName(node.Name); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", node.Name, err.Error()), logger)
		return
	}

	// Initialise status to NotReady on creation; the Agent will push heartbeats
	// to transition it to Ready once the connection is confirmed.
	if node.Status.State == "" {
		node.Status.State = v1.NodeStateNotReady
	}

	if err := h.store.CreateNode(traceCtx, &node); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node already exists: %s: %w", node.Name, store.ErrAlreadyExists), logger)
			return
		}
		logger.Error("CreateNode failed", zap.String("name", node.Name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	node.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Node"}
	handlerutil.WriteJSONResponse(w, http.StatusCreated, &node)
}

func (h *Handler) updateNode(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "updateNode")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	var node v1.Node
	if err := json.NewDecoder(r.Body).Decode(&node); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	// Reject requests where the body name does not match the URL path.
	if node.Name != "" && node.Name != name {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", node.Name,
				fmt.Sprintf("metadata.name %q does not match URL path %q", node.Name, name)), logger)
		return
	}
	node.Name = name

	// TODO: add metadata.resourceVersion / optimistic concurrency in a future PR.

	if err := h.store.UpdateNodeSpec(traceCtx, &node); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("UpdateNodeSpec failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	// Fetch the updated resource to return the full object (including status).
	updated, err := h.store.GetNode(traceCtx, name)
	if err != nil {
		logger.Error("GetNode failed after update", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, updated)
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "listNodes")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	nodes, err := h.store.ListNodes(traceCtx)
	if err != nil {
		logger.Error("ListNodes failed", zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	list := v1.NodeList{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "NodeList"},
		Items:    make([]v1.Node, len(nodes)),
	}
	for i, n := range nodes {
		list.Items[i] = *n
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, list)
}

func (h *Handler) getNode(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "getNode")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	node, err := h.store.GetNode(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetNode failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, node)
}

func (h *Handler) deleteNode(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "deleteNode")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	// Guard: reject deletion when projects are still assigned to this node.
	activePhases := []v1.ProjectPhase{
		v1.ProjectPhaseScheduled,
		v1.ProjectPhaseRunning,
		v1.ProjectPhaseTerminating,
	}
	projects, err := h.projectStore.ListProjectsByNodeRef(traceCtx, name, activePhases)
	if err != nil {
		logger.Error("ListProjectsByNodeRef failed during node delete",
			zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	if len(projects) > 0 {
		h.problemWriter.WriteError(traceCtx, w,
			fmt.Errorf("cannot delete node %q: %d project(s) still assigned: %w",
				name, len(projects), store.ErrAlreadyExists), logger)
		return
	}

	if err := h.store.DeleteNode(traceCtx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("DeleteNode failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
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
	traceCtx, span := h.tracer.Start(r.Context(), "heartbeat")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	// Fetch existing status so we can merge rather than overwrite.
	existing, err := h.store.GetNode(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetNode failed during heartbeat", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	var req heartbeatRequest
	// Body is optional; an empty / missing body is a pure timestamp update.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
			return
		}
	}

	// Validate state before persisting.
	if req.State != "" && !req.State.IsValid() {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("state", req.State,
				"invalid state "+string(req.State)+": must be one of Ready, NotReady, Draining"), logger)
		return
	}

	// Merge incoming fields into existing status.
	status := existing.Status
	status.LastHeartbeat = time.Now().UTC()
	if req.State != "" {
		status.State = req.State
	}
	// Merge Network field-by-field so that a heartbeat sending only AgentPort
	// does not clobber a previously-set IP (or vice versa).
	if req.Network.IP != "" {
		status.Network.IP = req.Network.IP
	}
	if req.Network.DNSName != "" {
		status.Network.DNSName = req.Network.DNSName
	}
	if req.Network.Mode != "" {
		status.Network.Mode = req.Network.Mode
	}
	if req.Network.AgentPort != 0 {
		status.Network.AgentPort = req.Network.AgentPort
	}
	if req.Network.Throughput != (v1.NodeThroughput{}) {
		status.Network.Throughput = req.Network.Throughput
	}
	if len(req.Capacity) > 0 {
		status.Capacity = req.Capacity
	}
	if len(req.Allocatable) > 0 {
		status.Allocatable = req.Allocatable
	}

	if err := h.store.UpdateNodeStatus(traceCtx, name, status); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("node not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("UpdateNodeStatus failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Debug("Heartbeat received", zap.String("node", name))
	w.WriteHeader(http.StatusNoContent)
}
