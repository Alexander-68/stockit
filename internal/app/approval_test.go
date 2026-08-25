package app

import (
	"encoding/json/v2"
	"net/http"
	"testing"
)

// seedApprovalFixture creates one supplier, one item and a draft purchase order
// with a single line worth totalMajor, and returns the purchase order id.
func seedApprovalFixture(t *testing.T, client *http.Client, token, baseURL, docNumber string, totalMajor float64) string {
	t.Helper()

	supplier := createRecord(t, client, token, baseURL, "suppliers", map[string]any{
		"sup_code": "RV-SUP-" + docNumber, "sup_name_en": "Approval Supplier", "sup_status": "Active",
	})
	item := createRecord(t, client, token, baseURL, "items", map[string]any{
		"itm_sku": "RV-ITM-" + docNumber, "itm_model": "APR-1", "itm_status": "Active",
	})
	purchaseOrder := createRecord(t, client, token, baseURL, "purchase_orders", map[string]any{
		"por_doc_number": docNumber, "por_doc_date": "2026-08-01", "sup_id": supplier,
		"por_status": "draft", "por_payment_status": "unpaid", "por_currency": "USD",
	})
	createRecord(t, client, token, baseURL, "po_components", map[string]any{
		"por_id": purchaseOrder, "itm_id": item, "poc_qty": 1, "poc_price": totalMajor, "poc_currency": "USD",
	})
	return purchaseOrder
}

func jsonNumberString(t *testing.T, value any) string {
	t.Helper()

	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode id: %v", err)
	}
	return string(encoded)
}

func TestPurchaseOrderApprovalAndStatusHistoryAPI(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	admin := apiLogin(t, client, ts.URL, "admin", "admin").Token

	createRecord(t, client, admin, ts.URL, "approval_rules", map[string]any{
		"apr_source_type": "purchase_order", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 10_000, "apr_status": "active",
	})
	purchaseOrder := seedApprovalFixture(t, client, admin, ts.URL, "RV-PO-WF", 1000)

	submitResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/submit", admin, nil)
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", submitResp.StatusCode)
	}
	var submission struct {
		Status     string           `json:"status"`
		TotalMinor int64            `json:"total_minor"`
		Approvals  []map[string]any `json:"approvals"`
	}
	decodeJSON(t, submitResp.Body, &submission)
	if submission.Status != "pending_approval" || submission.TotalMinor != 100_000 || len(submission.Approvals) != 1 {
		t.Fatalf("unexpected submission: %+v", submission)
	}

	approvalID := jsonNumberString(t, submission.Approvals[0]["apv_id"])
	decideResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/approvals/"+approvalID+"/decide", admin, map[string]any{
		"decision": "approved", "note": "budget confirmed",
	})
	if decideResp.StatusCode != http.StatusOK {
		t.Fatalf("decide status = %d, want 200", decideResp.StatusCode)
	}
	_ = decideResp.Body.Close()

	statusResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/status", admin, map[string]any{
		"por_status": "issued", "por_payment_status": "partially_paid", "note": "deposit wired",
	})
	if statusResp.StatusCode != http.StatusOK {
		t.Fatalf("status change = %d, want 200", statusResp.StatusCode)
	}
	var changed struct {
		PurchaseOrder map[string]any `json:"purchase_order"`
	}
	decodeJSON(t, statusResp.Body, &changed)
	if changed.PurchaseOrder["por_status"] != "issued" || changed.PurchaseOrder["por_payment_status"] != "partially_paid" {
		t.Fatalf("unexpected purchase order: %+v", changed.PurchaseOrder)
	}

	bad := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/status", admin, map[string]any{"por_status": "shipped"})
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("legacy status = %d, want 400", bad.StatusCode)
	}

	historyResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/po_status_history?parent_id="+purchaseOrder+"&sort=psh_id", admin, nil)
	if historyResp.StatusCode != http.StatusOK {
		t.Fatalf("history status = %d, want 200", historyResp.StatusCode)
	}
	var history struct {
		Rows []map[string]any `json:"rows"`
	}
	decodeJSON(t, historyResp.Body, &history)
	if len(history.Rows) != 4 {
		t.Fatalf("history rows = %d, want 4: %+v", len(history.Rows), history.Rows)
	}
	last := history.Rows[len(history.Rows)-1]
	if last["psh_status"] != "issued" || last["psh_note"] != "deposit wired" || last["usr_id"] == nil {
		t.Fatalf("unexpected last history row: %+v", last)
	}

	// The status history is an audit trail: it may be read, never written directly.
	write := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/po_status_history", admin, map[string]any{
		"por_id": purchaseOrder, "psh_status": "closed",
	})
	_ = write.Body.Close()
	if write.StatusCode != http.StatusForbidden {
		t.Fatalf("direct history write = %d, want 403", write.StatusCode)
	}
}

func TestPurchaseOrderBelowThresholdNeedsNoApproval(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	admin := apiLogin(t, client, ts.URL, "admin", "admin").Token

	createRecord(t, client, admin, ts.URL, "approval_rules", map[string]any{
		"apr_source_type": "purchase_order", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 1_000_000, "apr_status": "active",
	})
	purchaseOrder := seedApprovalFixture(t, client, admin, ts.URL, "RV-PO-SMALL", 12.5)

	submitResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/submit", admin, nil)
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", submitResp.StatusCode)
	}
	var submission struct {
		Status     string           `json:"status"`
		TotalMinor int64            `json:"total_minor"`
		Approvals  []map[string]any `json:"approvals"`
	}
	decodeJSON(t, submitResp.Body, &submission)
	// No rule matches this amount, so StockIt approves the order outright.
	if submission.Status != "approved" || submission.TotalMinor != 1250 || len(submission.Approvals) != 0 {
		t.Fatalf("unexpected submission: %+v", submission)
	}

	resubmit := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/submit", admin, nil)
	_ = resubmit.Body.Close()
	if resubmit.StatusCode != http.StatusBadRequest {
		t.Fatalf("resubmit status = %d, want 400", resubmit.StatusCode)
	}
}
