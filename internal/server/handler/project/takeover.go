package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/server/handler"
	"NYCU-SDC/caravanserai/internal/store"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	"go.uber.org/zap"
)

const maxTakeoverNoteLength = 1024

// takeoverRequest binds an operator's approval to one assignment and one
// decision. Clients fill the fence from the displayed recovery state.
type takeoverRequest struct {
	UID                  string `json:"uid"`
	NodeRef              string `json:"nodeRef"`
	AssignmentGeneration *int64 `json:"assignmentGeneration"`
	RequestID            string `json:"requestID"`
	Note                 string `json:"note,omitempty"`
}

// TakeoverResponse is the decision a takeover request resolved to.
type TakeoverResponse struct {
	Record   v1.TakeoverAuditRecord `json:"record"`
	Replayed bool                   `json:"replayed"`
}

// authorizeOperator requires an admin caller (ADR-0031). A takeover can leave
// a workload running on two Nodes, so a Node's own token must never reach it.
func (h *Handler) authorizeOperator(r *http.Request) error {
	return handler.RequireAdmin(r.Context())
}

// postTakeover handles POST /api/v1/projects/{name}/recovery/takeover.
//
// This is a risk-bearing override, not proof the old Node stopped: if that Node
// is alive, its workload runs alongside the new assignment until it reconnects
// and cleans up.
func (h *Handler) postTakeover(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "postTakeover")
	defer span.End()
	logger := h.logger.With(zap.String("project", r.PathValue("name")))
	name := r.PathValue("name")

	if err := h.authorizeOperator(r); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}

	var req takeoverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("body", nil, "invalid request body: "+err.Error()), logger)
		return
	}
	if req.RequestID == "" {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("requestID", req.RequestID, "requestID is required"), logger)
		return
	}
	if len(req.Note) > maxTakeoverNoteLength {
		h.problemWriter.WriteError(ctx, w,
			handlerutil.NewValidationError("note", nil, "note is too long"), logger)
		return
	}
	if req.UID == "" || req.NodeRef == "" || req.AssignmentGeneration == nil {
		h.problemWriter.WriteError(ctx, w,
			fmt.Errorf("%w: project %q: takeover requires a complete assignment fence", store.ErrStaleAssignment, name), logger)
		return
	}

	ref := store.AssignmentRef{UID: req.UID, NodeRef: req.NodeRef, Generation: *req.AssignmentGeneration}
	record, replayed, err := h.store.TakeoverProject(ctx, name, ref, store.TakeoverAudit{
		RequestID: req.RequestID,
		Caller:    operatorCaller(r),
		Note:      req.Note,
	})
	if err != nil {
		if !errors.Is(err, store.ErrStaleAssignment) && !errors.Is(err, store.ErrConflictState) && !errors.Is(err, store.ErrNotFound) {
			logger.Error("Operator takeover failed", zap.Error(err))
		}
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}

	logger.Warn("Operator takeover recorded",
		zap.String("node", req.NodeRef),
		zap.Int64("assignmentGeneration", ref.Generation),
		zap.String("outcome", string(record.Outcome)),
		zap.Bool("replayed", replayed))
	handlerutil.WriteJSONResponse(w, http.StatusOK, TakeoverResponse{Record: record, Replayed: replayed})
}

// getTakeoverAudit handles GET /api/v1/projects/{name}/recovery/takeover-audit.
func (h *Handler) getTakeoverAudit(w http.ResponseWriter, r *http.Request) {
	ctx, span := h.tracer.Start(r.Context(), "getTakeoverAudit")
	defer span.End()
	logger := h.logger.With(zap.String("project", r.PathValue("name")))

	if err := h.authorizeOperator(r); err != nil {
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	records, err := h.store.ListTakeoverAudit(ctx, r.PathValue("name"))
	if err != nil {
		logger.Error("Listing takeover audit failed", zap.Error(err))
		h.problemWriter.WriteError(ctx, w, err, logger)
		return
	}
	handlerutil.WriteJSONResponse(w, http.StatusOK, records)
}

// operatorCaller names the authenticated caller for the audit record. The
// break-glass token is shared, so "user:break-glass" names the credential, not
// a person.
func operatorCaller(r *http.Request) string {
	if p, ok := auth.FromContext(r.Context()); ok {
		return p.String()
	}
	return "unknown"
}
