package app

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"stockit/internal/store"
)

type apiDecideRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

type apiSetPOStatusRequest struct {
	Status        string `json:"por_status"`
	PaymentStatus string `json:"por_payment_status"`
	Note          string `json:"note"`
}

type apiSetPOStatusResponse struct {
	Table         string         `json:"table"`
	PurchaseOrder map[string]any `json:"purchase_order"`
}

// workflowStatus maps store workflow errors onto HTTP status codes so callers
// can tell "not allowed for your role" from "wrong state".
func workflowStatus(err error) int {
	switch {
	case errors.Is(err, store.ErrWorkflowForbidden):
		return http.StatusForbidden
	case errors.Is(err, store.ErrWorkflow):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func parseWorkflowID(raw string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func (s *Server) apiDecidePurchaseOrder(ctx context.Context, principal Principal, rawID, decision, note string) (store.ApprovalDecision, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_orders", true); err != nil {
		return store.ApprovalDecision{}, status, err
	}
	id, err := parseWorkflowID(rawID)
	if err != nil {
		return store.ApprovalDecision{}, http.StatusBadRequest, err
	}
	decision = strings.ToLower(strings.TrimSpace(decision))
	if decision != "approved" && decision != "rejected" {
		return store.ApprovalDecision{}, http.StatusBadRequest, fmt.Errorf("decision must be \"approved\" or \"rejected\"")
	}
	result, err := s.store.DecidePurchaseOrder(ctx, id, principal.UserID, decision == "approved", note)
	if err != nil {
		return store.ApprovalDecision{}, workflowStatus(err), err
	}
	return result, 0, nil
}

func (s *Server) apiSubmitPurchaseOrder(ctx context.Context, principal Principal, rawID string) (store.ApprovalSubmission, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_orders", true); err != nil {
		return store.ApprovalSubmission{}, status, err
	}
	id, err := parseWorkflowID(rawID)
	if err != nil {
		return store.ApprovalSubmission{}, http.StatusBadRequest, err
	}
	result, err := s.store.SubmitPurchaseOrder(ctx, id)
	if err != nil {
		return store.ApprovalSubmission{}, workflowStatus(err), err
	}
	return result, 0, nil
}

func (s *Server) apiSetPOStatus(ctx context.Context, principal Principal, rawID string, payload apiSetPOStatusRequest) (apiSetPOStatusResponse, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_orders", true); err != nil {
		return apiSetPOStatusResponse{}, status, err
	}
	id, err := parseWorkflowID(rawID)
	if err != nil {
		return apiSetPOStatusResponse{}, http.StatusBadRequest, err
	}
	row, err := s.store.SetPOStatus(ctx, id, payload.Status, payload.PaymentStatus, payload.Note)
	if err != nil {
		return apiSetPOStatusResponse{}, workflowStatus(err), err
	}
	return apiSetPOStatusResponse{Table: "purchase_orders", PurchaseOrder: row}, 0, nil
}

func (s *Server) handleAPISubmitPurchaseOrder(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	result, status, err := s.apiSubmitPurchaseOrder(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAPISetPOStatus(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var payload apiSetPOStatusRequest
	if err := json.UnmarshalRead(r.Body, &payload); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "invalid JSON body"})
		return
	}
	result, status, err := s.apiSetPOStatus(r.Context(), principal, r.PathValue("id"), payload)
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAPIDecidePurchaseOrder(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var payload apiDecideRequest
	if err := json.UnmarshalRead(r.Body, &payload); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "invalid JSON body"})
		return
	}
	result, status, err := s.apiDecidePurchaseOrder(r.Context(), principal, r.PathValue("id"), payload.Decision, payload.Note)
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}
