// Package overlay provides the HTTP handler for Headscale overlay
// administration (CARA-49).
//
// Routes registered:
//
//	POST   /api/v1/overlay/preauth-keys     — issue a Headscale pre-auth key
//	GET    /api/v1/overlay/nodes            — list Headscale overlay nodes
//	DELETE /api/v1/overlay/nodes/{name}     — revoke a node (Headscale + Cara store)
//
// caractl calls these endpoints; the server talks to Headscale, so operators
// never shell into the Headscale container. When the server has no Headscale
// admin credentials the endpoints return a clear "not configured" error.
package overlay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/headscale"
	serverhandler "NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

const (
	// defaultKeyTTL is the pre-auth key lifetime when a request omits ttl.
	defaultKeyTTL = 24 * time.Hour

	// storeDeleteAttempts is how many times the Cara-store delete is retried
	// during revoke to absorb a transient database blip before drift is
	// reported. Persistent failures are surfaced, not hidden.
	storeDeleteAttempts = 3
	storeDeleteBackoff  = 200 * time.Millisecond
)

// NodeDeleter is the subset of the node store revoke needs.
type NodeDeleter interface {
	DeleteNode(ctx context.Context, name string) error
}

// Handler implements apiserver.RouteRegistrar for overlay admin endpoints.
type Handler struct {
	logger        *zap.Logger
	hs            headscale.Client
	store         NodeDeleter
	user          string
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter
}

// NewHandler creates an overlay admin Handler. hs may be nil when the server
// was started without Headscale admin credentials; in that case every endpoint
// reports that overlay administration is not configured.
func NewHandler(logger *zap.Logger, hs headscale.Client, s NodeDeleter, user string, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		hs:            hs,
		store:         s,
		user:          user,
		tracer:        otel.Tracer("overlay/handler"),
		problemWriter: pw,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/overlay/preauth-keys", mid.HandlerFunc(h.createPreAuthKey))
	mux.HandleFunc("GET /api/v1/overlay/nodes", mid.HandlerFunc(h.listNodes))
	mux.HandleFunc("DELETE /api/v1/overlay/nodes/{name}", mid.HandlerFunc(h.revokeNode))
}

// ── handlers ────────────────────────────────────────────────────────────────

func (h *Handler) createPreAuthKey(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "createPreAuthKey")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	if !h.configured(traceCtx, w, logger) {
		return
	}

	var req v1.CreatePreAuthKeyRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
			return
		}
	}

	ttl := defaultKeyTTL
	if req.TTL != "" {
		parsed, err := time.ParseDuration(req.TTL)
		if err != nil || parsed <= 0 {
			h.problemWriter.WriteError(traceCtx, w,
				handlerutil.NewValidationError("ttl", req.TTL, "ttl must be a positive Go duration such as \"24h\""), logger)
			return
		}
		ttl = parsed
	}

	key, err := h.hs.CreatePreAuthKey(traceCtx, headscale.CreatePreAuthKeyRequest{
		User:       h.user,
		Reusable:   false,
		Ephemeral:  false,
		Expiration: time.Now().Add(ttl),
	})
	if err != nil {
		// Log the target node, never the key itself.
		logger.Error("CreatePreAuthKey failed", zap.String("node", req.Node), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	logger.Info("Issued Headscale pre-auth key", zap.String("node", req.Node), zap.String("user", h.user))
	handlerutil.WriteJSONResponse(w, http.StatusCreated, v1.PreAuthKeyResponse{
		Key:        key.Key,
		Expiration: key.Expiration,
	})
}

func (h *Handler) listNodes(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "listOverlayNodes")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	if !h.configured(traceCtx, w, logger) {
		return
	}

	nodes, err := h.hs.ListNodes(traceCtx)
	if err != nil {
		logger.Error("ListNodes failed", zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	list := v1.OverlayNodeList{Nodes: make([]v1.OverlayNode, 0, len(nodes))}
	for _, n := range nodes {
		list.Nodes = append(list.Nodes, v1.OverlayNode{
			ID:          n.ID,
			Name:        n.Name,
			IPAddresses: n.IPAddresses,
			Online:      n.Online,
		})
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, list)
}

// revokeNode removes a node from Headscale and then from the Cara node store.
//
// Ordering is deliberate: Headscale first (cutting overlay access), and if that
// fails the store is left untouched so both views stay consistent and the
// operator can retry cleanly. Only after Headscale removal succeeds is the store
// delete attempted; a persistent store failure there is the one unavoidable
// drift, and it is reported loudly rather than hidden.
func (h *Handler) revokeNode(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "revokeOverlayNode")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	if !h.configured(traceCtx, w, logger) {
		return
	}

	name := r.PathValue("name")

	// Resolve the Headscale node id by name. An absent node means Headscale is
	// already in the desired state, so removal is a no-op (idempotent).
	id, found, err := h.findHeadscaleNodeID(traceCtx, name)
	if err != nil {
		logger.Error("Revoke: list Headscale nodes failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	if found {
		if err := h.hs.DeleteNode(traceCtx, id); err != nil {
			// Headscale delete failed: do not touch the store. Both views stay
			// consistent and the operator can retry.
			logger.Error("Revoke: Headscale delete failed, store left untouched",
				zap.String("name", name), zap.Error(err))
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("revoke %q: delete from Headscale failed, Cara store left unchanged: %w", name, err), logger)
			return
		}
	}

	// Headscale is now clear. Remove the Cara store record, retrying briefly to
	// absorb a transient database error.
	storeErr := h.deleteFromStoreWithRetry(traceCtx, name)
	if storeErr != nil {
		// The one drift we cannot avoid: gone from Headscale, still in Cara.
		// Report it clearly; re-running revoke is safe and will retry the store
		// delete (Headscale delete is idempotent).
		msg := fmt.Sprintf("node %q removed from Headscale but Cara store delete failed; re-run revoke to retry: %v", name, storeErr)
		logger.Error("Revoke: store delete failed after Headscale removal (drift)",
			zap.String("name", name), zap.Error(storeErr))
		handlerutil.WriteJSONResponse(w, http.StatusOK, v1.RevokeNodeResponse{
			HeadscaleRemoved: true,
			StoreRemoved:     false,
			Drift:            true,
			Message:          msg,
		})
		return
	}

	logger.Info("Revoked overlay node", zap.String("name", name), zap.Bool("headscalePresent", found))
	handlerutil.WriteJSONResponse(w, http.StatusOK, v1.RevokeNodeResponse{
		HeadscaleRemoved: true,
		StoreRemoved:     true,
	})
}

// findHeadscaleNodeID returns the Headscale node id whose name matches, and
// whether such a node exists.
func (h *Handler) findHeadscaleNodeID(ctx context.Context, name string) (string, bool, error) {
	nodes, err := h.hs.ListNodes(ctx)
	if err != nil {
		return "", false, err
	}
	for _, n := range nodes {
		if n.Name == name {
			return n.ID, true, nil
		}
	}
	return "", false, nil
}

// deleteFromStoreWithRetry deletes the node from the Cara store. A missing
// record counts as success (idempotent). Transient errors are retried a few
// times; the last error is returned if they never clear.
func (h *Handler) deleteFromStoreWithRetry(ctx context.Context, name string) error {
	var lastErr error
	for attempt := 0; attempt < storeDeleteAttempts; attempt++ {
		err := h.store.DeleteNode(ctx, name)
		if err == nil || errors.Is(err, store.ErrNotFound) {
			return nil
		}
		lastErr = err
		if attempt < storeDeleteAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(storeDeleteBackoff):
			}
		}
	}
	return lastErr
}

// configured reports whether Headscale admin access is wired. When it is not,
// it writes a clear error and returns false.
func (h *Handler) configured(ctx context.Context, w http.ResponseWriter, logger *zap.Logger) bool {
	if h.hs == nil {
		h.problemWriter.WriteError(ctx, w,
			fmt.Errorf("%w: overlay administration requires headscale_api_url and headscale_api_key on cara-server", serverhandler.ErrServiceNotConfigured), logger)
		return false
	}
	return true
}
