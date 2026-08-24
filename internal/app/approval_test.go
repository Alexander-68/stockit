package app

import (
	"encoding/json/v2"
	"net/http"
	"testing"
)

// seedApprovalFixture creates one supplier, one item and a draft requisition
// with a single line worth totalMajor, and returns the requisition id.
func seedApprovalFixture(t *testing.T, client *http.Client, token, baseURL string, totalMajor float64) string {
	t.Helper()

	supplier := createRecord(t, client, token, baseURL, "suppliers", map[string]any{
		"sup_code": "RV-SUP-APR", "sup_name_en": "Approval Supplier", "sup_status": "Active",
	})
	item := createRecord(t, client, token, baseURL, "items", map[string]any{
		"itm_sku": "RV-ITM-APR", "itm_model": "APR-1", "itm_status": "Active",
	})
	requisition := createRecord(t, client, token, baseURL, "purchase_requisitions", map[string]any{
		"prq_doc_number": "RV-PRQ-01", "prq_date": "2026-08-01", "sup_id": supplier,
		"prq_status": "draft", "prq_currency": "USD",
	})
	createRecord(t, client, token, baseURL, "prq_components", map[string]any{
		"prq_id": requisition, "itm_id": item, "prc_qty": 1, "prc_price": totalMajor, "prc_currency": "USD",
	})
	return requisition
}

func TestRequisitionApprovalWorkflow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	admin := apiLogin(t, client, ts.URL, "admin", "admin").Token

	createRecord(t, client, admin, ts.URL, "approval_rules", map[string]any{
		"apr_source_type": "purchase_requisition", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 50_000, "apr_status": "active",
	})
	requisition := seedApprovalFixture(t, client, admin, ts.URL, 900)

	submitResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_requisitions/"+requisition+"/submit", admin, nil)
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", submitResp.StatusCode)
	}
	var submission struct {
		Status     string           `json:"status"`
		TotalMinor int64            `json:"total_minor"`
		Approvals  []map[string]any `json:"approvals"`
	}
	decodeJSON(t, submitResp.Body, &submission)
	if submission.Status != "submitted" || submission.TotalMinor != 90_000 || len(submission.Approvals) != 1 {
		t.Fatalf("unexpected submission: %+v", submission)
	}

	approvalID := jsonNumberString(t, submission.Approvals[0]["apv_id"])

	userToken := apiLogin(t, client, ts.URL, "user", "user").Token
	wrongRole := doAPI(t, client, http.MethodPost, ts.URL+"/api/approvals/"+approvalID+"/decide", userToken, map[string]any{"decision": "approved"})
	_ = wrongRole.Body.Close()
	if wrongRole.StatusCode != http.StatusForbidden {
		t.Fatalf("decide as wrong role status = %d, want 403", wrongRole.StatusCode)
	}

	decideResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/approvals/"+approvalID+"/decide", admin, map[string]any{
		"decision": "approved", "note": "within budget",
	})
	if decideResp.StatusCode != http.StatusOK {
		t.Fatalf("decide status = %d, want 200", decideResp.StatusCode)
	}
	var decision struct {
		RequisitionStatus  string `json:"requisition_status"`
		RemainingApprovals int    `json:"remaining_approvals"`
	}
	decodeJSON(t, decideResp.Body, &decision)
	if decision.RequisitionStatus != "approved" || decision.RemainingApprovals != 0 {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	poResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_requisitions/"+requisition+"/purchase_order", admin, map[string]any{
		"por_doc_number": "RV-PO-FROM-PRQ",
	})
	if poResp.StatusCode != http.StatusCreated {
		t.Fatalf("create purchase order status = %d, want 201", poResp.StatusCode)
	}
	var created struct {
		PurchaseOrder map[string]any `json:"purchase_order"`
	}
	decodeJSON(t, poResp.Body, &created)
	if created.PurchaseOrder["por_doc_number"] != "RV-PO-FROM-PRQ" || created.PurchaseOrder["por_status"] != "draft" {
		t.Fatalf("unexpected purchase order: %+v", created.PurchaseOrder)
	}

	again := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_requisitions/"+requisition+"/purchase_order", admin, nil)
	_ = again.Body.Close()
	if again.StatusCode != http.StatusBadRequest {
		t.Fatalf("second conversion status = %d, want 400", again.StatusCode)
	}
}

func TestRequisitionBelowThresholdNeedsNoApproval(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	admin := apiLogin(t, client, ts.URL, "admin", "admin").Token

	createRecord(t, client, admin, ts.URL, "approval_rules", map[string]any{
		"apr_source_type": "purchase_requisition", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 1_000_000, "apr_status": "active",
	})
	requisition := seedApprovalFixture(t, client, admin, ts.URL, 12.5)

	submitResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_requisitions/"+requisition+"/submit", admin, nil)
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", submitResp.StatusCode)
	}
	var submission struct {
		Status     string           `json:"status"`
		TotalMinor int64            `json:"total_minor"`
		Approvals  []map[string]any `json:"approvals"`
	}
	decodeJSON(t, submitResp.Body, &submission)
	if submission.Status != "approved" || submission.TotalMinor != 1250 || len(submission.Approvals) != 0 {
		t.Fatalf("unexpected submission: %+v", submission)
	}

	resubmit := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_requisitions/"+requisition+"/submit", admin, nil)
	_ = resubmit.Body.Close()
	if resubmit.StatusCode != http.StatusBadRequest {
		t.Fatalf("resubmit status = %d, want 400", resubmit.StatusCode)
	}
}

func jsonNumberString(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode id: %v", err)
	}
	return string(encoded)
}
