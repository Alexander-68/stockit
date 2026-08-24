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
		{http.MethodPost, "/api/tables/suppliers", http.StatusForbidden},
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
