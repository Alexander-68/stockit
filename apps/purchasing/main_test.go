package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
)

func loggedInClient(t *testing.T, appServer *httptest.Server) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	body, err := json.Marshal(loginRequest{LoginName: "user", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Post(appServer.URL+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("login returned %d", response.StatusCode)
	}
	return client
}

func newFakeStockIt(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/auth/login" {
			writeJSON(w, http.StatusOK, stockitLoginResponse{Token: "stockit-token", User: "user"})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/api/me" {
			writeJSON(w, http.StatusOK, map[string]any{"user": "user", "role": "user", "approval_limit_minor": 250000})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			_, _ = w.Write([]byte(".stockit-login-card{}"))
			return
		}
		if r.Header.Get("Authorization") != "Bearer stockit-token" {
			t.Errorf("missing bearer token on %s %s", r.Method, r.URL.Path)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no token"})
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/tables/purchase_orders":
			payload, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(payload), "PO-1") {
				t.Errorf("create payload not forwarded: %s", payload)
			}
			writeJSON(w, http.StatusCreated, map[string]any{"table": "purchase_orders", "row": map[string]any{"por_id": 7, "por_doc_number": "PO-1"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/purchase_orders/9/submit":
			writeJSON(w, http.StatusOK, map[string]any{"source_id": 9, "status": "pending_approval"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/purchase_orders/9/status":
			payload, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(payload), "issued") {
				t.Errorf("status change not forwarded: %s", payload)
			}
			writeJSON(w, http.StatusOK, map[string]any{"table": "purchase_orders"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/purchase_orders/9/approve":
			payload, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(payload), "approved") {
				t.Errorf("decision not forwarded: %s", payload)
			}
			writeJSON(w, http.StatusOK, map[string]any{"status": "approved"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/tables/po_components":
			if r.URL.Query().Get("parent_id") != "7" {
				t.Errorf("parent_id query not forwarded: %s", r.URL.RawQuery)
			}
			writeJSON(w, http.StatusOK, map[string]any{"rows": []map[string]any{}, "has_more": false})
		default:
			t.Errorf("unexpected StockIt request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestProxyForwardsWhitelistedTables(t *testing.T) {
	stockit := newFakeStockIt(t)
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	client := loggedInClient(t, appServer)

	response, err := client.Post(appServer.URL+"/api/tables/purchase_orders", "application/json", strings.NewReader(`{"por_doc_number":"PO-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("create PO returned %d", response.StatusCode)
	}
	var created struct {
		Row map[string]any `json:"row"`
	}
	if err := json.UnmarshalRead(response.Body, &created); err != nil || created.Row["por_id"] == nil {
		t.Fatalf("create response not forwarded: %v %v", err, created)
	}

	listResponse, err := client.Get(appServer.URL + "/api/tables/po_components?sort=poc_id&limit=200&offset=0&parent_id=7")
	if err != nil {
		t.Fatal(err)
	}
	defer listResponse.Body.Close()
	if listResponse.StatusCode != http.StatusOK {
		t.Fatalf("list po_components returned %d", listResponse.StatusCode)
	}
}

func TestProxyRejectsUnlistedTablesMethodsAndAnonymous(t *testing.T) {
	stockit := newFakeStockIt(t)
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	client := loggedInClient(t, appServer)

	cases := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/api/tables/users", http.StatusForbidden},
		{http.MethodPost, "/api/tables/quotes", http.StatusForbidden},
		{http.MethodDelete, "/api/tables/items/1", http.StatusForbidden},
		{http.MethodGet, "/api/tables/purchase_orders/1/extra", http.StatusForbidden},
	}
	for _, testCase := range cases {
		request, err := http.NewRequest(testCase.method, appServer.URL+testCase.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != testCase.want {
			t.Errorf("%s %s returned %d, want %d", testCase.method, testCase.path, response.StatusCode, testCase.want)
		}
	}

	anonymous, err := http.Get(appServer.URL + "/api/tables/purchase_orders")
	if err != nil {
		t.Fatal(err)
	}
	_ = anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous proxy request returned %d, want 401", anonymous.StatusCode)
	}
}

// TestAssetsProxyIsPublic keeps the shared StockIt stylesheet reachable before login so the
// app's sign-in page renders with StockIt's own look.
func TestAssetsProxyIsPublic(t *testing.T) {
	stockit := newFakeStockIt(t)
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()

	response, err := http.Get(appServer.URL + "/assets/app/app.css")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("assets proxy returned %d, want 200", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "stockit-login-card") {
		t.Fatalf("assets proxy did not forward StockIt CSS: %s", body)
	}
}

// TestWorkflowProxyForwardsWhitelistedActions covers the approval endpoints, which
// live outside /api/tables and so need their own whitelist.
func TestWorkflowProxyForwardsWhitelistedActions(t *testing.T) {
	stockit := newFakeStockIt(t)
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	client := loggedInClient(t, appServer)

	decide, err := client.Post(appServer.URL+"/api/purchase_orders/9/approve", "application/json", strings.NewReader(`{"decision":"approved"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = decide.Body.Close()
	if decide.StatusCode != http.StatusOK {
		t.Fatalf("approve returned %d, want 200", decide.StatusCode)
	}

	for _, action := range []string{"submit", "status"} {
		response, err := client.Post(appServer.URL+"/api/purchase_orders/9/"+action, "application/json", strings.NewReader(`{"por_status":"issued"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatalf("purchase order %s returned %d, want 200", action, response.StatusCode)
		}
	}

	blockedPO, err := client.Post(appServer.URL+"/api/purchase_orders/9/cancel", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = blockedPO.Body.Close()
	if blockedPO.StatusCode != http.StatusForbidden {
		t.Fatalf("unlisted purchase order action returned %d, want 403", blockedPO.StatusCode)
	}

	anonymous, err := http.Post(appServer.URL+"/api/purchase_orders/9/submit", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = anonymous.Body.Close()
	if anonymous.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous workflow request returned %d, want 401", anonymous.StatusCode)
	}
}

// TestLoginCarriesApprovalLimit guards the signing limit reaching the browser:
// the UI picks Approve or Submit from it, and it is read once at login.
func TestLoginCarriesApprovalLimit(t *testing.T) {
	stockit := newFakeStockIt(t)
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	client := loggedInClient(t, appServer)

	for _, path := range []string{"/login", "/api/me"} {
		var response *http.Response
		var err error
		if path == "/login" {
			response, err = client.Post(appServer.URL+path, "application/json", strings.NewReader(`{"login_name":"a","password":"b"}`))
		} else {
			response, err = client.Get(appServer.URL + path)
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if !strings.Contains(string(body), `"approval_limit_minor":250000`) {
			t.Fatalf("%s did not carry the approval limit: %s", path, body)
		}
	}
}
