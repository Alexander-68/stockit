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

type apiDecideApprovalRequest struct {
	Decision string `json:"decision"`
	Note     string `json:"note"`
}

type apiCreatePORequest struct {
	DocNumber  string `json:"por_doc_number"`
	SupplierID *int64 `json:"sup_id"`
}

type apiCreatePOResponse struct {
	RequisitionID  int64          `json:"requisition_id"`
	PurchaseOrder  map[string]any `json:"purchase_order"`
	PurchaseOrders string         `json:"table"`
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

func (s *Server) apiSubmitRequisition(ctx context.Context, principal Principal, rawID string) (store.RequisitionSubmission, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_requisitions", true); err != nil {
		return store.RequisitionSubmission{}, status, err
	}
	id, err := parseWorkflowID(rawID)
	if err != nil {
		return store.RequisitionSubmission{}, http.StatusBadRequest, err
	}
	result, err := s.store.SubmitRequisition(ctx, id)
	if err != nil {
		return store.RequisitionSubmission{}, workflowStatus(err), err
	}
	return result, 0, nil
}

func (s *Server) apiDecideApproval(ctx context.Context, principal Principal, rawID, decision, note string) (store.ApprovalDecision, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "approvals", false); err != nil {
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
	result, err := s.store.DecideApproval(ctx, id, principal.Role, principal.UserID, decision == "approved", note)
	if err != nil {
		return store.ApprovalDecision{}, workflowStatus(err), err
	}
	return result, 0, nil
}

func (s *Server) apiCreatePOFromRequisition(ctx context.Context, principal Principal, rawID, docNumber string, supplierID *int64) (apiCreatePOResponse, int, error) {
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_requisitions", true); err != nil {
		return apiCreatePOResponse{}, status, err
	}
	if _, status, err := s.resolveTableForRole(principal.Role, "purchase_orders", true); err != nil {
		return apiCreatePOResponse{}, status, err
	}
	id, err := parseWorkflowID(rawID)
	if err != nil {
		return apiCreatePOResponse{}, http.StatusBadRequest, err
	}
	purchaseOrderID, err := s.store.CreatePOFromRequisition(ctx, id, docNumber, supplierID, principal.UserID)
	if err != nil {
		return apiCreatePOResponse{}, workflowStatus(err), err
	}
	row, err := s.store.Get(ctx, "purchase_orders", strconv.FormatInt(purchaseOrderID, 10))
	if err != nil {
		return apiCreatePOResponse{}, http.StatusInternalServerError, err
	}
	return apiCreatePOResponse{RequisitionID: id, PurchaseOrder: row, PurchaseOrders: "purchase_orders"}, 0, nil
}

func (s *Server) handleAPISubmitRequisition(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	result, status, err := s.apiSubmitRequisition(r.Context(), principal, r.PathValue("id"))
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAPIDecideApproval(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	var payload apiDecideApprovalRequest
	if err := json.UnmarshalRead(r.Body, &payload); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "invalid JSON body"})
		return
	}
	result, status, err := s.apiDecideApproval(r.Context(), principal, r.PathValue("id"), payload.Decision, payload.Note)
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAPICreatePOFromRequisition(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	payload := apiCreatePORequest{}
	if r.ContentLength > 0 {
		if err := json.UnmarshalRead(r.Body, &payload); err != nil {
			s.writeJSON(w, http.StatusBadRequest, apiErrorResponse{Error: "invalid JSON body"})
			return
		}
	}
	result, status, err := s.apiCreatePOFromRequisition(r.Context(), principal, r.PathValue("id"), payload.DocNumber, payload.SupplierID)
	if err != nil {
		s.writeJSON(w, status, apiErrorResponse{Error: err.Error()})
		return
	}
	s.writeJSON(w, http.StatusCreated, result)
}
