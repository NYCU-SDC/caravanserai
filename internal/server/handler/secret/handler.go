// Package secret provides the HTTP handler for Secret resources.
//
// Routes registered:
//
//	POST   /api/v1/secrets          — create a Secret
//	PUT    /api/v1/secrets/{name}   — full-spec update (credential rotation)
//	GET    /api/v1/secrets          — list Secrets
//	GET    /api/v1/secrets/{name}   — get a single Secret
//	DELETE /api/v1/secrets/{name}   — delete a Secret
//
// The API returns Secret values in plaintext — the Agent needs them to resolve
// EnvVar.valueFrom.secretKeyRef (CARA-57). Value redaction is a CLI-presentation
// concern only and is not enforced here.
package secret

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

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

// Handler implements apiserver.RouteRegistrar for Secret endpoints.
type Handler struct {
	logger        *zap.Logger
	store         store.SecretStore
	tracer        trace.Tracer
	problemWriter *problem.HttpWriter
}

// NewHandler creates a Secret Handler.
func NewHandler(logger *zap.Logger, s store.SecretStore, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		store:         s,
		tracer:        otel.Tracer("secret/handler"),
		problemWriter: pw,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("POST /api/v1/secrets", mid.HandlerFunc(h.createSecret))
	mux.HandleFunc("PUT /api/v1/secrets/{name}", mid.HandlerFunc(h.updateSecret))
	mux.HandleFunc("GET /api/v1/secrets", mid.HandlerFunc(h.listSecrets))
	mux.HandleFunc("GET /api/v1/secrets/{name}", mid.HandlerFunc(h.getSecret))
	mux.HandleFunc("DELETE /api/v1/secrets/{name}", mid.HandlerFunc(h.deleteSecret))
}

// validateSecretSpec rejects specs that would break secretKeyRef resolution in
// CARA-57: a data item must carry a non-empty key, and keys must be unique
// within the Secret (resolution looks up by key, so duplicates are ambiguous).
func validateSecretSpec(spec v1.SecretSpec) error {
	if len(spec.Data) == 0 {
		return handlerutil.NewValidationError("spec.data", nil, "spec.data must contain at least one key/value pair")
	}
	seen := make(map[string]bool, len(spec.Data))
	for _, item := range spec.Data {
		if item.Key == "" {
			return handlerutil.NewValidationError("spec.data[].key", nil, "each data item must have a non-empty key")
		}
		if seen[item.Key] {
			return handlerutil.NewValidationError("spec.data[].key", item.Key, "duplicate data key: "+item.Key)
		}
		seen[item.Key] = true
	}
	return nil
}

func (h *Handler) createSecret(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "createSecret")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	var secret v1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	if secret.Name == "" {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", nil, "metadata.name is required"), logger)
		return
	}
	if err := v1.ValidateName(secret.Name); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", secret.Name, err.Error()), logger)
		return
	}
	if err := v1.ValidateNamespace(secret.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", secret.Namespace, err.Error()), logger)
		return
	}
	if secret.Namespace == "" {
		secret.Namespace = v1.DefaultNamespace
	}
	if err := validateSecretSpec(secret.Spec); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	if err := h.store.CreateSecret(traceCtx, &secret); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("secret already exists: %s: %w", secret.Name, store.ErrAlreadyExists), logger)
			return
		}
		logger.Error("CreateSecret failed", zap.String("name", secret.Name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	secret.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Secret"}
	handlerutil.WriteJSONResponse(w, http.StatusCreated, &secret)
}

func (h *Handler) updateSecret(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "updateSecret")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	var secret v1.Secret
	if err := json.NewDecoder(r.Body).Decode(&secret); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}

	// Reject requests where the body name does not match the URL path.
	if secret.Name != "" && secret.Name != name {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.name", secret.Name,
				fmt.Sprintf("metadata.name %q does not match URL path %q", secret.Name, name)), logger)
		return
	}
	secret.Name = name

	if err := v1.ValidateNamespace(secret.Namespace); err != nil {
		h.problemWriter.WriteError(traceCtx, w,
			handlerutil.NewValidationError("metadata.namespace", secret.Namespace, err.Error()), logger)
		return
	}
	if secret.Namespace == "" {
		secret.Namespace = v1.DefaultNamespace
	}
	if err := validateSecretSpec(secret.Spec); err != nil {
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	if err := h.store.UpdateSecret(traceCtx, &secret); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("secret not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("UpdateSecret failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	secret.TypeMeta = v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "Secret"}
	handlerutil.WriteJSONResponse(w, http.StatusOK, &secret)
}

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "listSecrets")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	secrets, err := h.store.ListSecrets(traceCtx)
	if err != nil {
		logger.Error("ListSecrets failed", zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	list := v1.SecretList{
		TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "SecretList"},
		Items:    make([]v1.Secret, len(secrets)),
	}
	for i, s := range secrets {
		list.Items[i] = *s
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, list)
}

func (h *Handler) getSecret(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "getSecret")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	secret, err := h.store.GetSecret(traceCtx, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("secret not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("GetSecret failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, secret)
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "deleteSecret")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")
	if err := h.store.DeleteSecret(traceCtx, name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			h.problemWriter.WriteError(traceCtx, w,
				fmt.Errorf("secret not found: %s: %w", name, store.ErrNotFound), logger)
			return
		}
		logger.Error("DeleteSecret failed", zap.String("name", name), zap.Error(err))
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
