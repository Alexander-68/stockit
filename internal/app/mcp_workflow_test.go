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
	for _, name := range []string{mcpToolSubmitRequisition, mcpToolDecideApproval, mcpToolRequisitionToPO} {
		if !slices.Contains(tools, name) {
			t.Fatalf("admin tools/list missing %q: %v", name, tools)
		}
	}

	createRecord(t, client, loginResp.Token, ts.URL, "approval_rules", map[string]any{
		"apr_source_type": "purchase_requisition", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 10_000, "apr_status": "active",
	})
	requisition := seedApprovalFixture(t, client, loginResp.Token, ts.URL, 250)

	submitResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolSubmitRequisition, map[string]any{
		"requisition_id": requisition,
	})
	submission, ok := submitResult["structuredContent"].(map[string]any)
	if !ok || submission["status"] != "submitted" {
		t.Fatalf("submit tool payload unexpected: %+v", submitResult)
	}
	approvals, ok := submission["approvals"].([]any)
	if !ok || len(approvals) != 1 {
		t.Fatalf("submit tool created %v approval steps, want 1", submission["approvals"])
	}
	approvalID := jsonNumberString(t, approvals[0].(map[string]any)["apv_id"])

	decideResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolDecideApproval, map[string]any{
		"approval_id": approvalID, "decision": "approved",
	})
	decision, ok := decideResult["structuredContent"].(map[string]any)
	if !ok || decision["requisition_status"] != "approved" {
		t.Fatalf("decide tool payload unexpected: %+v", decideResult)
	}

	poResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolRequisitionToPO, map[string]any{
		"requisition_id": requisition, "por_doc_number": "RV-MCP-PO",
	})
	poPayload, ok := poResult["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("purchase order tool payload unexpected: %+v", poResult)
	}
	purchaseOrder := poPayload["purchase_order"].(map[string]any)
	if purchaseOrder["por_doc_number"] != "RV-MCP-PO" {
		t.Fatalf("purchase order not created from requisition: %+v", purchaseOrder)
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
