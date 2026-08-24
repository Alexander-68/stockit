package app

import (
	"net/http"
	"net/url"
	"testing"
)

func TestListRecordsServerSideFilters(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	token := apiLogin(t, client, ts.URL, "admin", "admin").Token

	for _, order := range []map[string]any{
		{"por_doc_number": "RV-F-2025", "por_doc_date": "2025-06-01", "por_status": "draft", "por_note": "old draft"},
		{"por_doc_number": "RV-F-2026A", "por_doc_date": "2026-03-10", "por_status": "draft", "por_note": "spare parts"},
		{"por_doc_number": "RV-F-2026B", "por_doc_date": "2026-07-20", "por_status": "sent", "por_note": "tooling"},
	} {
		createRecord(t, client, token, ts.URL, "purchase_orders", order)
	}

	listDocNumbers := func(query string) []string {
		t.Helper()
		resp := getWithHeaders(t, client, ts.URL+"/api/tables/purchase_orders?"+query, map[string]string{
			"Authorization": "Bearer " + token,
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d for %q, want 200", resp.StatusCode, query)
		}
		var payload apiTableListResponse
		decodeJSON(t, resp.Body, &payload)
		numbers := make([]string, 0, len(payload.Rows))
		for _, row := range payload.Rows {
			if value, ok := row["por_doc_number"].(string); ok {
				numbers = append(numbers, value)
			}
		}
		return numbers
	}

	if got := listDocNumbers("sort=por_doc_date&from.por_doc_date=2026-01-01"); len(got) != 2 {
		t.Fatalf("date range filter returned %v, want the two 2026 orders", got)
	}
	if got := listDocNumbers("sort=por_doc_date&from.por_doc_date=2026-01-01&to.por_doc_date=2026-06-30"); len(got) != 1 || got[0] != "RV-F-2026A" {
		t.Fatalf("bounded range returned %v, want RV-F-2026A only", got)
	}
	if got := listDocNumbers("filter.por_status=sent"); len(got) != 1 || got[0] != "RV-F-2026B" {
		t.Fatalf("status filter returned %v, want RV-F-2026B only", got)
	}
	if got := listDocNumbers("q=" + url.QueryEscape("spare")); len(got) != 1 || got[0] != "RV-F-2026A" {
		t.Fatalf("search returned %v, want RV-F-2026A only", got)
	}
	// A LIKE wildcard in the search term must be matched literally, not expanded.
	if got := listDocNumbers("q=" + url.QueryEscape("%")); len(got) != 0 {
		t.Fatalf("wildcard search returned %v, want no rows", got)
	}

	bad := getWithHeaders(t, client, ts.URL+"/api/tables/purchase_orders?filter.not_a_column=1", map[string]string{
		"Authorization": "Bearer " + token,
	})
	_ = bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown filter column status = %d, want 400", bad.StatusCode)
	}
}
