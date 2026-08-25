package app

import (
	"slices"
	"testing"
)

func TestMCPApprovalToolsAndListFilters(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, client, ts.URL, loginResp.Token)

	tools := mcpListTools(t, client, ts.URL, loginResp.Token, sessionID)
	for _, name := range []string{mcpToolSubmitPO, mcpToolApprovePO, mcpToolSetPOStatus} {
		if !slices.Contains(tools, name) {
			t.Fatalf("admin tools/list missing %q: %v", name, tools)
		}
	}

	purchaseOrder := seedApprovalFixture(t, client, loginResp.Token, ts.URL, "RV-MCP-PO", 250)

	submitResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolSubmitPO, map[string]any{
		"purchase_order_id": purchaseOrder,
	})
	submission, ok := submitResult["structuredContent"].(map[string]any)
	if !ok || submission["status"] != "pending_approval" {
		t.Fatalf("submit tool payload unexpected: %+v", submitResult)
	}
	decideResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolApprovePO, map[string]any{
		"purchase_order_id": purchaseOrder, "decision": "approved",
	})
	decision, ok := decideResult["structuredContent"].(map[string]any)
	if !ok || decision["status"] != "approved" {
		t.Fatalf("decide tool payload unexpected: %+v", decideResult)
	}

	statusResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolSetPOStatus, map[string]any{
		"purchase_order_id": purchaseOrder, "por_status": "issued",
		"por_payment_status": "partially_paid", "note": "deposit wired",
	})
	statusPayload, ok := statusResult["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("status tool payload unexpected: %+v", statusResult)
	}
	changed := statusPayload["purchase_order"].(map[string]any)
	if changed["por_status"] != "issued" || changed["por_payment_status"] != "partially_paid" {
		t.Fatalf("status tool did not apply the change: %+v", changed)
	}

	filtered := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolListRecords, map[string]any{
		"table":  "purchase_orders",
		"filter": map[string]any{"por_doc_number": "RV-MCP-PO"},
	})
	listPayload := filtered["structuredContent"].(map[string]any)
	rows := listPayload["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("filtered list returned %d rows, want 1", len(rows))
	}
}
