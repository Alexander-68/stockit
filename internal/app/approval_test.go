package app

import (
	"encoding/json/v2"
	"net/http"
	"strings"
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

	purchaseOrder := seedApprovalFixture(t, client, admin, ts.URL, "RV-PO-WF", 1000)

	// The buyer's limit falls short, so the order is handed on instead.
	buyer := createRecord(t, client, admin, ts.URL, "users", map[string]any{
		"usr_login_name": "limited", "usr_password": "limited-pass", "usr_role": "user",
		"usr_approval_limit_minor": 10_000,
	})
	_ = buyer
	limited := apiLogin(t, client, ts.URL, "limited", "limited-pass").Token
	overLimit := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/approve", limited, map[string]any{"decision": "approved"})
	_ = overLimit.Body.Close()
	if overLimit.StatusCode != http.StatusForbidden {
		t.Fatalf("approve over limit = %d, want 403", overLimit.StatusCode)
	}

	submitResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/submit", limited, nil)
	if submitResp.StatusCode != http.StatusOK {
		t.Fatalf("submit status = %d, want 200", submitResp.StatusCode)
	}
	var submission struct {
		Status     string `json:"status"`
		TotalMinor int64  `json:"total_minor"`
	}
	decodeJSON(t, submitResp.Body, &submission)
	if submission.Status != "pending_approval" || submission.TotalMinor != 100_000 {
		t.Fatalf("unexpected submission: %+v", submission)
	}

	// admin carries the seeded ceiling, so it can decide anything.
	decideResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/approve", admin, map[string]any{
		"decision": "approved", "note": "budget confirmed",
	})
	if decideResp.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", decideResp.StatusCode)
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

// TestApprovalWithinOwnLimitIsOneStep covers the common small-company path: the
// buyer's own signing limit covers the order, so approval is a single call.
func TestApprovalWithinOwnLimitIsOneStep(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	admin := apiLogin(t, client, ts.URL, "admin", "admin").Token

	createRecord(t, client, admin, ts.URL, "users", map[string]any{
		"usr_login_name": "buyer", "usr_password": "buyer-pass", "usr_role": "user",
		"usr_approval_limit_minor": 100_000,
	})
	buyer := apiLogin(t, client, ts.URL, "buyer", "buyer-pass").Token

	me := doAPI(t, client, http.MethodGet, ts.URL+"/api/me", buyer, nil)
	var principal struct {
		ApprovalLimitMinor int64 `json:"approval_limit_minor"`
	}
	decodeJSON(t, me.Body, &principal)
	if principal.ApprovalLimitMinor != 100_000 {
		t.Fatalf("/api/me limit = %d, want 100000", principal.ApprovalLimitMinor)
	}

	purchaseOrder := seedApprovalFixture(t, client, buyer, ts.URL, "RV-PO-SMALL", 12.5)

	// Straight from draft to approved, no submit step in between.
	approve := doAPI(t, client, http.MethodPost, ts.URL+"/api/purchase_orders/"+purchaseOrder+"/approve", buyer, map[string]any{
		"decision": "approved", "note": "within my limit",
	})
	if approve.StatusCode != http.StatusOK {
		t.Fatalf("approve status = %d, want 200", approve.StatusCode)
	}
	var decision struct {
		Status     string `json:"status"`
		TotalMinor int64  `json:"total_minor"`
	}
	decodeJSON(t, approve.Body, &decision)
	if decision.Status != "approved" || decision.TotalMinor != 1250 {
		t.Fatalf("unexpected decision: %+v", decision)
	}

	// Writing the decided status directly must stay refused.
	sneak := doAPI(t, client, http.MethodPut, ts.URL+"/api/tables/purchase_orders/"+purchaseOrder, buyer, map[string]any{
		"por_doc_number": "RV-PO-SMALL", "por_status": "approved",
	})
	_ = sneak.Body.Close()
	if sneak.StatusCode != http.StatusBadRequest {
		t.Fatalf("direct status write = %d, want 400", sneak.StatusCode)
	}
}

// TestUserNamesAreReadableByAnyPrincipal covers the id-to-name map external apps
// use to label who changed a status. It must not leak anything else, and it must
// work for a non-admin, who cannot read the users table itself.
func TestUserNamesAreReadableByAnyPrincipal(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	guest := apiLogin(t, client, ts.URL, "guest", "guest").Token

	blocked := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/users", guest, nil)
	_ = blocked.Body.Close()
	if blocked.StatusCode != http.StatusForbidden {
		t.Fatalf("users table for guest = %d, want 403", blocked.StatusCode)
	}

	response := doAPI(t, client, http.MethodGet, ts.URL+"/api/users/names", guest, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("user names status = %d, want 200", response.StatusCode)
	}
	body := readBody(t, response.Body)
	for _, want := range []string{`"usr_login_name":"admin"`, `"usr_login_name":"guest"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("user names missing %s: %s", want, body)
		}
	}
	// Nothing beyond the id and the name may travel with it.
	for _, leak := range []string{"usr_password", "usr_role", "usr_approval_limit_minor"} {
		if strings.Contains(body, leak) {
			t.Fatalf("user names leaked %s: %s", leak, body)
		}
	}
}
