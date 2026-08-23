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
//	POST   /api/v1/nodes/{name}/probe      — server→agent reachability probe (via agentdialer)
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/agentdialer"
	"NYCU-SDC/caravanserai/internal/server/controller"
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
	keys          PreAuthKeyValidator
	dialer        agentdialer.Dialer
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter
}

type ProjectLister interface {
	ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []v1.ProjectPhase) ([]*v1.Project, error)
}

// overlayOfflineThreshold is the heartbeat age beyond which a Node is reported
// Offline. It reuses the same window the NodeHealthController uses to mark a
// Node NotReady, so State and OverlayStatus never disagree.
const overlayOfflineThreshold = controller.NodeHeartbeatTimeout

// computeOverlayStatus derives a Node's overlay reachability from heartbeat
// freshness. It is a pure function so the tri-state logic stays unit-testable:
//   - no overlay IP or no heartbeat ever  -> Unknown
//   - heartbeat older than threshold      -> Offline
//   - otherwise                           -> Online
//
// A heartbeat exactly at the threshold counts as Online, matching the
// NodeHealthController's age <= timeout Ready boundary.
func computeOverlayStatus(overlayIP string, lastHeartbeat, now time.Time, threshold time.Duration) v1.OverlayStatus {
	if overlayIP == "" || lastHeartbeat.IsZero() {
		return v1.OverlayStatusUnknown
	}
	if now.Sub(lastHeartbeat) > threshold {
		return v1.OverlayStatusOffline
	}
	return v1.OverlayStatusOnline
}

// setOverlayStatus fills the computed OverlayStatus on a Node before it is
// returned to a client. This is the only place OverlayStatus is set; the
// heartbeat path never writes it.
func setOverlayStatus(node *v1.Node, now time.Time) {
	node.Status.Network.OverlayStatus = computeOverlayStatus(
		node.Status.Network.OverlayIP, node.Status.LastHeartbeat, now, overlayOfflineThreshold)
}

// PreAuthKeyValidator binds a heartbeat's pre-auth key reference to the Cara
// Node it was issued for (CARA-68). It may be nil, in which case heartbeat
// identity binding is skipped entirely.
type PreAuthKeyValidator interface {
	GetPreAuthKeyByHash(ctx context.Context, keyHash string) (*store.PreAuthKey, error)
	MarkPreAuthKeyUsed(ctx context.Context, keyHash, usedByIP string, usedAt time.Time) error
}

func NewHandler(logger *zap.Logger, s store.NodeStore, ps ProjectLister, keys PreAuthKeyValidator, dialer agentdialer.Dialer, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		store:         s,
		projectStore:  ps,
		keys:          keys,
		dialer:        dialer,
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
	mux.HandleFunc("POST /api/v1/nodes/{name}/probe", mid.HandlerFunc(h.probe))
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

	if err := v1.ValidateNamespace(node.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", node.Namespace, err.Error()), logger)
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

	if err := v1.ValidateNamespace(node.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", node.Namespace, err.Error()), logger)
		return
	}

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
	now := time.Now()
	for i, n := range nodes {
		list.Items[i] = *n
		setOverlayStatus(&list.Items[i], now)
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
	setOverlayStatus(node, time.Now())
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
	// KeyRef is the hex SHA-256 of the pre-auth key the agent joined with. When
	// present, the server binds this heartbeat to the Cara Node the key was
	// issued for (CARA-68). It is a hash, not the key itself.
	KeyRef string `json:"keyRef,omitempty"`
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
	// does not clobber a previously-set OverlayIP (or vice versa).
	if req.Network.OverlayIP != "" {
		status.Network.OverlayIP = req.Network.OverlayIP
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

	// Bind the heartbeat to the Cara Node its pre-auth key was issued for
	// (CARA-68). Rejections here stop the status update so a mismatched or
	// expired key cannot mutate a node it does not own.
	if ok := h.validatePreAuthKey(traceCtx, w, logger, name, req.KeyRef, status.Network.OverlayIP); !ok {
		return
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

// validatePreAuthKey binds a heartbeat's pre-auth key reference to the intended
// Cara Node. It returns true when the heartbeat may proceed and false when it
// has already written a rejection to w.
//
// Policy (CARA-68):
//   - No keyRef, or no validator wired: nothing to bind, proceed.
//   - keyRef maps to a different Cara Node: reject (a key issued for node A must
//     not claim node B). This is the security property this ticket adds.
//   - keyRef is expired: reject.
//   - keyRef maps to this node and is still active: mark it used, recording the
//     overlay IP that consumed it.
//   - keyRef maps to this node and is already used: proceed (idempotent — agents
//     resend the ref on every heartbeat).
//   - keyRef is unknown to the store: proceed without binding. Keys may be
//     created out of band (operator/dev bootstrap, design §6.3); the wrong-node
//     protection above is what this ticket guarantees.
//
// The keyRef is a hash; only its short prefix is ever logged.
func (h *Handler) validatePreAuthKey(ctx context.Context, w http.ResponseWriter, logger *zap.Logger, name, keyRef, overlayIP string) bool {
	if keyRef == "" || h.keys == nil {
		return true
	}

	refPrefix := keyRef
	if len(refPrefix) > 8 {
		refPrefix = refPrefix[:8]
	}

	mapping, err := h.keys.GetPreAuthKeyByHash(ctx, keyRef)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			logger.Debug("Heartbeat pre-auth key not mapped; skipping identity binding",
				zap.String("node", name), zap.String("key_ref_prefix", refPrefix))
			return true
		}
		logger.Error("GetPreAuthKeyByHash failed during heartbeat",
			zap.String("node", name), zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return false
	}

	if mapping.CaraNodeName != name {
		logger.Warn("Heartbeat pre-auth key claims wrong node",
			zap.String("node", name), zap.String("key_ref_prefix", refPrefix))
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("keyRef", refPrefix,
				fmt.Sprintf("pre-auth key does not authorize node %q", name)), logger)
		return false
	}

	if !mapping.Expiration.IsZero() && !mapping.Expiration.After(time.Now()) {
		logger.Warn("Heartbeat pre-auth key expired",
			zap.String("node", name), zap.String("key_ref_prefix", refPrefix))
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("keyRef", refPrefix, "pre-auth key has expired"), logger)
		return false
	}

	if mapping.State == store.PreAuthKeyStateActive {
		if err := h.keys.MarkPreAuthKeyUsed(ctx, keyRef, overlayIP, time.Now().UTC()); err != nil {
			logger.Error("MarkPreAuthKeyUsed failed during heartbeat",
				zap.String("node", name), zap.Error(err))
			h.problemWriter.WriteError(ctx, w, err, logger)
			return false
		}
		logger.Info("Pre-auth key consumed",
			zap.String("node", name), zap.String("key_ref_prefix", refPrefix), zap.String("overlay_ip", overlayIP))
	}

	return true
}
