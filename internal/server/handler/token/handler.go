// Package token provides the HTTP handler that issues and revokes API tokens
// (CARA-137, ADR-0031).
//
// Routes registered:
//
//	POST   /api/v1/tokens        — issue a token; the response is the only time it is shown
//	GET    /api/v1/tokens        — list tokens, without the tokens themselves
//	DELETE /api/v1/tokens/{id}   — revoke a token
//
// Every route is admin-only: a Node token must never be able to mint another
// identity.
package token

import (
	"encoding/json"
	"net/http"
	"strconv"

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
}

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
