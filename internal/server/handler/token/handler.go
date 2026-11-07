// Package token provides the HTTP handler that issues and revokes API tokens
// (CARA-137, ADR-0031).
//
// Routes registered:
//
//	POST   /api/v1/tokens        — issue a token; the response is the only time it is shown
//	GET    /api/v1/tokens        — list tokens, without the tokens themselves
//	DELETE /api/v1/tokens/{id}   — revoke a token
//
//	POST   /api/v1/nodes/{name}/join-tokens — issue a one-time join token for a Node (CARA-138)
//	POST   /api/v1/nodes/{name}/join        — an Agent trades a join token for a Node token
//	DELETE /api/v1/nodes/{name}/tokens      — revoke a Node's tokens and unused join tokens
//
// Every route but join is admin-only: a Node token must never be able to mint
// another identity. Join is authenticated by the join token itself, which the
// authentication middleware lets through to it.
package token

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// Handler implements apiserver.RouteRegistrar for token endpoints.
type Handler struct {
	logger        *zap.Logger
	store         store.APITokenStore
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter
}

// NewHandler creates a token Handler.
func NewHandler(logger *zap.Logger, s store.APITokenStore, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		store:         s,
		tracer:        otel.Tracer("token/handler"),
		problemWriter: pw,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/tokens", mid.HandlerFunc(h.createToken))
	mux.HandleFunc("GET /api/v1/tokens", mid.HandlerFunc(h.listTokens))
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", mid.HandlerFunc(h.revokeToken))
	mux.HandleFunc("POST /api/v1/nodes/{name}/join-tokens", mid.HandlerFunc(h.createJoinToken))
	mux.HandleFunc("POST /api/v1/nodes/{name}/join", mid.HandlerFunc(h.joinNode))
	mux.HandleFunc("DELETE /api/v1/nodes/{name}/tokens", mid.HandlerFunc(h.revokeNodeTokens))
}

// DefaultJoinTokenTTL is how long a join token works when the request names no
// TTL: long enough to install an Agent, short enough that a forgotten one stops
// mattering.
const DefaultJoinTokenTTL = time.Hour

// MaxJoinTokenTTL bounds a join token's lifetime.
const MaxJoinTokenTTL = 7 * 24 * time.Hour

func (h *Handler) createToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "createToken")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	if err := handler.RequireAdmin(ctx); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	caller, _ := auth.FromContext(ctx)

	var req v1.APITokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}
	if !req.Kind.Valid() {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("kind", req.Kind, `kind must be "node" or "user"`), logger)
		return
	}
	if err := v1.ValidateName(req.Name); err != nil {
		h.problemWriter.WriteError(ctx, w, handlerutil.NewValidationError("name", req.Name, err.Error()), logger)
		return
	}
	if req.Kind == v1.APITokenKindUser && req.Name == auth.BreakGlassName {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("name", req.Name, "the break-glass name is reserved"), logger)
		return
	}

	token, err := auth.NewToken()
	if err != nil {
		logger.Error("Failed to generate a token", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	issued, err := h.store.CreateAPIToken(ctx, req.Kind, req.Name, auth.HashToken(token), caller.String())
	if err != nil {
		logger.Error("CreateAPIToken failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Issued an API token", zap.Int64("id", issued.ID),
		zap.String("principal", string(issued.Kind)+":"+issued.Name), zap.String("by", caller.String()))
	handlerutil.WriteJSONResponse(w, http.StatusCreated, v1.APITokenCreated{APIToken: issued, Token: token})
}

func (h *Handler) listTokens(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "listTokens")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	if err := handler.RequireAdmin(ctx); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	tokens, err := h.store.ListAPITokens(ctx)
	if err != nil {
		logger.Error("ListAPITokens failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, tokens)
}

func (h *Handler) revokeToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "revokeToken")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	if err := handler.RequireAdmin(ctx); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	caller, _ := auth.FromContext(ctx)

	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("id", r.PathValue("id"), "id must be an integer"), logger)
		return
	}
	if err := h.store.RevokeAPIToken(ctx, id); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Revoked an API token", zap.Int64("id", id), zap.String("by", caller.String()))
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) createJoinToken(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "createJoinToken")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	if err := handler.RequireAdmin(ctx); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	caller, _ := auth.FromContext(ctx)

	name := r.PathValue("name")
	if err := v1.ValidateName(name); err != nil {
		h.problemWriter.WriteError(ctx, w, handlerutil.NewValidationError("name", name, err.Error()), logger)
		return
	}
	var req v1.NodeJoinTokenRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.problemWriter.WriteError(ctx, w,
				handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
			return
		}
	}
	ttl := DefaultJoinTokenTTL
	if req.TTL != "" {
		d, err := time.ParseDuration(req.TTL)
		if err != nil || d <= 0 || d > MaxJoinTokenTTL {
			h.problemWriter.WriteError(ctx, w,
				handlerutil.NewValidationError("ttl", req.TTL, "ttl must be a duration between 1s and "+MaxJoinTokenTTL.String()), logger)
			return
		}
		ttl = d
	}

	token, err := auth.NewToken()
	if err != nil {
		logger.Error("Failed to generate a join token", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	issued, err := h.store.CreateNodeJoinToken(ctx, name, auth.HashToken(token), caller.String(), time.Now().Add(ttl))
	if err != nil {
		logger.Error("CreateNodeJoinToken failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Issued a Node join token", zap.Int64("id", issued.ID), zap.String("node", name),
		zap.Time("expiresAt", issued.ExpiresAt), zap.String("by", caller.String()))
	handlerutil.WriteJSONResponse(w, http.StatusCreated, v1.NodeJoinTokenCreated{NodeJoinToken: issued, Token: token})
}

// joinNode trades a join token for a Node token. The middleware attaches no
// principal here: the join token is the credential, checked against the Node
// it was issued for.
func (h *Handler) joinNode(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "joinNode")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	name := r.PathValue("name")
	joinToken, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	joinToken = strings.TrimSpace(joinToken)
	if !ok || joinToken == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="cara"`)
		h.problemWriter.WriteError(ctx, w, handler.ErrInvalidJoinToken, logger)
		return
	}

	nodeToken, err := auth.NewToken()
	if err != nil {
		logger.Error("Failed to generate a Node token", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	issued, err := h.store.RedeemNodeJoinToken(ctx, auth.HashToken(joinToken), name, auth.HashToken(nodeToken), time.Now())
	switch {
	case errors.Is(err, store.ErrNotFound):
		logger.Warn("Refused a Node join with an invalid join token", zap.String("node", name))
		w.Header().Set("WWW-Authenticate", `Bearer realm="cara"`)
		h.problemWriter.WriteError(ctx, w, handler.ErrInvalidJoinToken, logger)
		return
	case errors.Is(err, store.ErrWrongNode):
		logger.Warn("Refused a Node join for another Node's join token", zap.String("node", name), zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	case err != nil:
		logger.Error("RedeemNodeJoinToken failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Node joined", zap.String("node", name), zap.Int64("tokenID", issued.ID), zap.String("by", issued.CreatedBy))
	handlerutil.WriteJSONResponse(w, http.StatusCreated, v1.NodeJoined{APIToken: issued, Token: nodeToken})
}

func (h *Handler) revokeNodeTokens(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "revokeNodeTokens")
	defer span.End()
	logger := logutil.WithContext(ctx, h.logger)

	if err := handler.RequireAdmin(ctx); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	caller, _ := auth.FromContext(ctx)

	name := r.PathValue("name")
	if err := v1.ValidateName(name); err != nil {
		h.problemWriter.WriteError(ctx, w, handlerutil.NewValidationError("name", name, err.Error()), logger)
		return
	}
	nodes, joins, err := h.store.RevokeNodeTokens(ctx, name)
	if err != nil {
		logger.Error("RevokeNodeTokens failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	logger.Info("Revoked a Node's tokens", zap.String("node", name), zap.Int("nodeTokens", nodes),
		zap.Int("joinTokens", joins), zap.String("by", caller.String()))
	handlerutil.WriteJSONResponse(w, http.StatusOK, v1.NodeTokensRevoked{NodeTokens: nodes, JoinTokens: joins})
}
