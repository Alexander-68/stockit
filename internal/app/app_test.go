package app

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"flag"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var (
	testKeepDB     = flag.Bool("stockit-keep-db", false, "keep populated SQLite test databases after test completion")
	testDBDir      = flag.String("stockit-db-dir", "", "directory for kept SQLite test databases when -stockit-keep-db is set")
	testDBPathFlag = flag.String("stockit-db-path", "", "exact SQLite database path for kept test data; overrides -stockit-db-dir")
)

func TestLoginDashboardAndBearerAPI(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	resp := postForm(t, client, ts.URL+"/login", url.Values{
		"login_name": {"admin"},
		"password":   {"admin"},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}

	location, err := resp.Location()
	if err != nil {
		t.Fatalf("login location: %v", err)
	}
	if location.Path != "/" {
		t.Fatalf("login redirect path = %q, want /", location.Path)
	}

	dashboardResp := get(t, client, ts.URL+"/")
	body := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if !strings.Contains(body, "StockIt") || !strings.Contains(body, "Customers") {
		t.Fatalf("dashboard body missing expected content: %s", body)
	}

	apiResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/me", sessionCookieValue(t, client, ts.URL), nil)
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("api me status = %d, want 200", apiResp.StatusCode)
	}

	var payload apiResponse
	decodeJSON(t, apiResp.Body, &payload)
	if payload.User != "admin" || payload.Role != "admin" {
		t.Fatalf("unexpected api me payload: %+v", payload)
	}
}

func TestInvalidLoginRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	resp := postForm(t, client, ts.URL+"/login", url.Values{
		"login_name": {"admin"},
		"password":   {"wrong"},
	})
	body := readBody(t, resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("login status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(body, "Invalid login credentials.") {
		t.Fatalf("unexpected login body: %s", body)
	}
}

func TestAPIAuthLoginAndTableCatalog(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	if loginResp.Token == "" {
		t.Fatal("api login token is empty")
	}
	if loginResp.User != "admin" || loginResp.Role != "admin" {
		t.Fatalf("unexpected api login payload: %+v", loginResp)
	}

	catalogResp := getWithHeaders(t, client, ts.URL+"/api/tables", map[string]string{
		"Authorization": "Bearer " + loginResp.Token,
	})
	if catalogResp.StatusCode != http.StatusOK {
		t.Fatalf("table catalog status = %d, want 200", catalogResp.StatusCode)
	}

	var catalog apiTableListEnvelope
	decodeJSON(t, catalogResp.Body, &catalog)
	if len(catalog.Tables) == 0 {
		t.Fatal("expected non-empty table catalog")
	}

	foundCustomers := false
	for _, table := range catalog.Tables {
		if table.Name == "customers" {
			foundCustomers = true
			if !table.CanWrite {
				t.Fatal("customers should be writable for admin")
			}
		}
	}
	if !foundCustomers {
		t.Fatalf("customers table missing from catalog: %+v", catalog.Tables)
	}

	schemaResp := getWithHeaders(t, client, ts.URL+"/api/tables/customers/schema", map[string]string{
		"Authorization": "Bearer " + loginResp.Token,
	})
	if schemaResp.StatusCode != http.StatusOK {
		t.Fatalf("table schema status = %d, want 200", schemaResp.StatusCode)
	}

	var schema apiTableSchemaEnvelope
	decodeJSON(t, schemaResp.Body, &schema)
	if schema.Table.Name != "customers" || len(schema.Table.Fields) == 0 {
		t.Fatalf("unexpected schema payload: %+v", schema)
	}
}

func TestAPIRejectsUnknownFieldsAndInvalidIDs(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")

	unknownFieldResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/customers", loginResp.Token, map[string]any{
		"cus_name_en": "Bad Payload",
		"unknown":     "value",
	})
	if unknownFieldResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field create status = %d, want 400", unknownFieldResp.StatusCode)
	}
	var createErr apiErrorResponse
	decodeJSON(t, unknownFieldResp.Body, &createErr)
	if !strings.Contains(createErr.Error, "unknown or read-only field") {
		t.Fatalf("unexpected create error: %+v", createErr)
	}

	invalidIDResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/customers/not-an-int", loginResp.Token, nil)
	if invalidIDResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid id status = %d, want 400", invalidIDResp.StatusCode)
	}
	var idErr apiErrorResponse
	decodeJSON(t, invalidIDResp.Body, &idErr)
	if !strings.Contains(idErr.Error, "invalid id") {
		t.Fatalf("unexpected invalid id error: %+v", idErr)
	}

	wrongTypeResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/customers", loginResp.Token, map[string]any{
		"cus_name_en": "Typed Wrong",
		"cus_status":  true,
	})
	if wrongTypeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong type create status = %d, want 400", wrongTypeResp.StatusCode)
	}
	var typeErr apiErrorResponse
	decodeJSON(t, wrongTypeResp.Body, &typeErr)
	if !strings.Contains(typeErr.Error, "must be a string") {
		t.Fatalf("unexpected type error: %+v", typeErr)
	}
}

func TestMCPRequiresAuthentication(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	resp := doMCP(t, client, ts.URL+"/mcp", "", "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "stockit-test", "version": "1.0.0"},
			"capabilities":    map[string]any{},
		},
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated mcp status = %d, want 401", resp.StatusCode)
	}
}

func TestMCPWorksOverHTTPAndHTTPS(t *testing.T) {
	for _, serverFactory := range []struct {
		name string
		new  func(*testing.T) *testServer
	}{
		{name: "http", new: newTestServer},
		{name: "https", new: newTLSTestServer},
	} {
		t.Run(serverFactory.name, func(t *testing.T) {
			ts := serverFactory.new(t)
			defer ts.Close()

			client := newServerHTTPClient(t, ts.server)
			loginResp := apiLogin(t, client, ts.URL, "admin", "admin")

			initializeResp := doMCP(t, client, ts.URL+"/mcp", loginResp.Token, "", map[string]any{
				"jsonrpc": "2.0",
				"id":      1,
				"method":  "initialize",
				"params": map[string]any{
					"protocolVersion": "2025-03-26",
					"clientInfo":      map[string]any{"name": "stockit-test", "version": "1.0.0"},
					"capabilities":    map[string]any{},
				},
			})
			if initializeResp.StatusCode != http.StatusOK {
				t.Fatalf("initialize status = %d, want 200", initializeResp.StatusCode)
			}
			sessionID := initializeResp.Header.Get("Mcp-Session-Id")
			if sessionID == "" {
				t.Fatal("missing MCP session id")
			}

			var initializePayload map[string]any
			decodeJSON(t, initializeResp.Body, &initializePayload)
			if initializePayload["result"] == nil {
				t.Fatalf("missing initialize result: %+v", initializePayload)
			}

			listToolsResp := doMCP(t, client, ts.URL+"/mcp", loginResp.Token, sessionID, map[string]any{
				"jsonrpc": "2.0",
				"id":      2,
				"method":  "tools/list",
				"params":  map[string]any{},
			})
			if listToolsResp.StatusCode != http.StatusOK {
				t.Fatalf("tools/list status = %d, want 200", listToolsResp.StatusCode)
			}
			var toolsPayload map[string]any
			decodeJSON(t, listToolsResp.Body, &toolsPayload)

			toolNames := extractMCPToolNames(t, toolsPayload)
			if !slices.Contains(toolNames, mcpToolListTables) {
				t.Fatalf("tools/list missing %q: %v", mcpToolListTables, toolNames)
			}

			callResp := doMCP(t, client, ts.URL+"/mcp", loginResp.Token, sessionID, map[string]any{
				"jsonrpc": "2.0",
				"id":      3,
				"method":  "tools/call",
				"params": map[string]any{
					"name":      mcpToolListTables,
					"arguments": map[string]any{},
				},
			})
			if callResp.StatusCode != http.StatusOK {
				t.Fatalf("tools/call status = %d, want 200", callResp.StatusCode)
			}

			var callPayload map[string]any
			decodeJSON(t, callResp.Body, &callPayload)
			structured, ok := callPayload["result"].(map[string]any)["structuredContent"].(map[string]any)
			if !ok {
				t.Fatalf("missing structured MCP result: %+v", callPayload)
			}
			tables, ok := structured["tables"].([]any)
			if !ok || len(tables) == 0 {
				t.Fatalf("unexpected MCP table payload: %+v", structured)
			}
		})
	}
}

func TestHTTPSSetsStrictTransportAndSecureCookie(t *testing.T) {
	ts := newTLSTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)

	loginPageResp := get(t, client, ts.URL+"/login")
	_ = loginPageResp.Body.Close()
	if got := loginPageResp.Header.Get("Strict-Transport-Security"); got == "" {
		t.Fatal("expected HSTS header on HTTPS response")
	}

	resp := doJSON(t, client, http.MethodPost, ts.URL+"/api/auth/login", nil, apiLoginRequest{
		LoginName: "admin",
		Password:  "admin",
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api login status = %d, want 200", resp.StatusCode)
	}

	var sessionCookie *http.Cookie
	for _, cookie := range resp.Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("session cookie missing")
	}
	if !sessionCookie.Secure {
		t.Fatal("session cookie should be Secure over HTTPS")
	}
}

func TestGuestWriteForbiddenAndUserCannotReadUsersTable(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	guestClient := newHTTPClient(t)
	login(t, guestClient, ts.URL, "guest", "guest")

	writeResp := postForm(t, guestClient, ts.URL+"/tables/customers/save", url.Values{
		"cus_name_en": {"Guest Attempt"},
	})
	if writeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("guest write status = %d, want 403", writeResp.StatusCode)
	}

	userClient := newHTTPClient(t)
	login(t, userClient, ts.URL, "user", "user")

	panelResp := get(t, userClient, ts.URL+"/tables/users?limit=30")
	if panelResp.StatusCode != http.StatusForbidden {
		t.Fatalf("user users-table status = %d, want 403", panelResp.StatusCode)
	}

	apiResp := doAPI(t, userClient, http.MethodGet, ts.URL+"/api/tables/users", sessionCookieValue(t, userClient, ts.URL), nil)
	if apiResp.StatusCode != http.StatusForbidden {
		t.Fatalf("user users-api status = %d, want 403", apiResp.StatusCode)
	}

	adminClient := newHTTPClient(t)
	login(t, adminClient, ts.URL, "admin", "admin")
	adminToken := sessionCookieValue(t, adminClient, ts.URL)
	_ = createRecord(t, adminClient, adminToken, ts.URL, "customers", map[string]any{
		"cus_name_en": "Admin-Owned Customer",
		"cus_status":  "Active",
	})
	customersPanelResp := get(t, adminClient, ts.URL+"/tables/customers?limit=30")
	customersPanelBody := readBody(t, customersPanelResp.Body)
	if customersPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("customers panel status = %d, want 200", customersPanelResp.StatusCode)
	}
	if !strings.Contains(customersPanelBody, ">admin<") {
		t.Fatalf("customers panel should show username for user_id list cells: %s", customersPanelBody)
	}
	if strings.Contains(customersPanelBody, "1 | admin | admin") {
		t.Fatalf("customers panel should not show expanded users reference labels in list cells: %s", customersPanelBody)
	}
	if !strings.Contains(customersPanelBody, `data-row-delete-confirm="Delete record from Customers?`) ||
		!strings.Contains(customersPanelBody, `Admin-Owned Customer | admin | Active`) {
		t.Fatalf("customers panel should include record-specific delete confirmation details: %s", customersPanelBody)
	}
}

func TestCrossOriginWriteRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/tables/customers/save", strings.NewReader("cus_name_en=Blocked"))
	if err != nil {
		t.Fatalf("new write request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("HX-Request", "true")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do write request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %d, want 403", resp.StatusCode)
	}
}

func TestUserIDIsAutomaticAndNotSelectableInFormsOrWrites(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "user", "user")
	token := sessionCookieValue(t, client, ts.URL)

	createFormResp := get(t, client, ts.URL+"/tables/customers/form")
	createFormBody := readBody(t, createFormResp.Body)
	if createFormResp.StatusCode != http.StatusOK {
		t.Fatalf("create form status = %d, want 200", createFormResp.StatusCode)
	}
	if strings.Contains(createFormBody, `<select class="stockit-select mt-2 block w-full" name="usr_id"`) || strings.Contains(createFormBody, `<input class="stockit-input mt-2 block w-full" type="text" name="usr_id"`) {
		t.Fatalf("create form should not expose usr_id as a selectable/editable field: %s", createFormBody)
	}
	if !strings.Contains(createFormBody, `type="hidden" name="usr_id" value="2"`) {
		t.Fatalf("create form should keep usr_id as hidden current-user context: %s", createFormBody)
	}

	customerID := createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Owned By User",
		"cus_status":  "Active",
		"usr_id":      1,
	})

	customerResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/customers/"+customerID, token, nil)
	if customerResp.StatusCode != http.StatusOK {
		t.Fatalf("customer api status = %d, want 200", customerResp.StatusCode)
	}
	var created apiResponse
	decodeJSON(t, customerResp.Body, &created)
	if fmt.Sprint(created.Row["usr_id"]) != "2" {
		t.Fatalf("created customer usr_id = %v, want 2", created.Row["usr_id"])
	}

	editFormResp := get(t, client, ts.URL+"/tables/customers/form?id="+customerID)
	editFormBody := readBody(t, editFormResp.Body)
	if editFormResp.StatusCode != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", editFormResp.StatusCode)
	}
	if strings.Contains(editFormBody, `<select class="stockit-select mt-2 block w-full" name="usr_id"`) || strings.Contains(editFormBody, `<input class="stockit-input mt-2 block w-full" type="text" name="usr_id"`) {
		t.Fatalf("edit form should not expose usr_id as a selectable/editable field: %s", editFormBody)
	}
	if !strings.Contains(editFormBody, `type="hidden" name="usr_id" value="2"`) {
		t.Fatalf("edit form should preserve usr_id as a hidden creator field: %s", editFormBody)
	}

	updateRecord(t, client, token, ts.URL, "customers", customerID, map[string]any{
		"cus_name_en": "Still Owned By User",
		"usr_id":      1,
	})

	updatedResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/customers/"+customerID, token, nil)
	if updatedResp.StatusCode != http.StatusOK {
		t.Fatalf("updated customer api status = %d, want 200", updatedResp.StatusCode)
	}
	var updated apiResponse
	decodeJSON(t, updatedResp.Body, &updated)
	if fmt.Sprint(updated.Row["usr_id"]) != "2" {
		t.Fatalf("updated customer usr_id = %v, want 2", updated.Row["usr_id"])
	}
}

func TestModalFormUsesCompactAutogrowTextareas(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	formResp := get(t, client, ts.URL+"/tables/customers/form")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("form status = %d, want 200", formResp.StatusCode)
	}
	if !strings.Contains(formBody, `id="stockit-modal-form"`) || !strings.Contains(formBody, `data-stockit-modal-form="true"`) {
		t.Fatalf("modal form should expose the modal keyboard hook: %s", formBody)
	}
	for _, removedText := range []string{"Record Editor", "Create record", "Edit record"} {
		if strings.Contains(formBody, removedText) {
			t.Fatalf("modal form should remove legacy header copy %q: %s", removedText, formBody)
		}
	}
	if !strings.Contains(formBody, `class="stockit-modal-actions"`) || !strings.Contains(formBody, `>Cancel</button>`) || !strings.Contains(formBody, `>Save</button>`) {
		t.Fatalf("modal form should render header actions for cancel/save: %s", formBody)
	}
	if !strings.Contains(formBody, `class="stockit-field-caption"`) {
		t.Fatalf("modal form should render compact floating field captions: %s", formBody)
	}

	addressFieldPattern := regexp.MustCompile(`(?s)<textarea[^>]*name="cus_address_en"[^>]*rows="1"[^>]*data-stockit-autogrow="true"`)
	if !addressFieldPattern.MatchString(formBody) {
		t.Fatalf("customer address field should render as a compact autogrow textarea: %s", formBody)
	}

	customerID := createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Modal Layout Review",
		"cus_status":  "Active",
	})

	editResp := get(t, client, ts.URL+"/tables/customers/form?id="+customerID)
	editBody := readBody(t, editResp.Body)
	if editResp.StatusCode != http.StatusOK {
		t.Fatalf("edit form status = %d, want 200", editResp.StatusCode)
	}
	if !strings.Contains(editBody, `>Delete</button>`) {
		t.Fatalf("edit modal should render delete in the header action row: %s", editBody)
	}
	if !strings.Contains(editBody, `hx-confirm="Delete record from Customers?`) ||
		!strings.Contains(editBody, `Modal Layout Review | admin | Active`) {
		t.Fatalf("edit modal delete button should include record-specific confirmation text: %s", editBody)
	}
}

func TestCRUDImportSortingAndPasswordHiding(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	zuluID := createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Zulu Co",
		"cus_phone":   "1000",
		"cus_status":  "Active",
	})
	_ = createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Acme Co",
		"cus_phone":   "2000",
		"cus_status":  "Hold",
	})

	updateRecord(t, client, token, ts.URL, "customers", zuluID, map[string]any{
		"cus_name_en": "Zulu Prime",
		"cus_phone":   "9999",
		"cus_status":  "Under Review",
	})

	importResp := postCSV(t, client, ts.URL+"/tables/customers/import", "customers.csv", ""+
		"cus_name_en,cus_phone,cus_status\n"+
		"Mango Co,3000,Active\n")
	if importResp.StatusCode != http.StatusNoContent {
		t.Fatalf("import status = %d, want 204", importResp.StatusCode)
	}

	listResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/customers?sort=cus_name_en&desc=true&limit=50", token, nil)
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listResp.StatusCode)
	}
	var listPayload apiResponse
	decodeJSON(t, listResp.Body, &listPayload)

	if len(listPayload.Rows) < 3 {
		t.Fatalf("expected at least 3 customers, got %d", len(listPayload.Rows))
	}
	if fmt.Sprint(listPayload.Rows[0]["cus_name_en"]) != "Zulu Prime" {
		t.Fatalf("expected descending sort to start with Zulu Prime, got %+v", listPayload.Rows[0])
	}

	panelResp := get(t, client, ts.URL+"/tables/customers?limit=40&sort=cus_name_en&desc=true")
	panelBody := readBody(t, panelResp.Body)
	for _, expected := range []string{"Zulu Prime", "Acme Co", "Mango Co"} {
		if !strings.Contains(panelBody, expected) {
			t.Fatalf("customers panel missing %q: %s", expected, panelBody)
		}
	}

	usersResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/users?limit=20", token, nil)
	if usersResp.StatusCode != http.StatusOK {
		t.Fatalf("users api status = %d, want 200", usersResp.StatusCode)
	}
	var usersPayload apiResponse
	decodeJSON(t, usersResp.Body, &usersPayload)
	for _, row := range usersPayload.Rows {
		if _, ok := row["usr_password"]; ok {
			t.Fatalf("password hash leaked in users api row: %+v", row)
		}
	}
}

func TestBOMCascadeAndLastAdminDeleteGuard(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	finalItemID := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "FG-001",
		"itm_model":        "Final Widget",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	partItemID := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "PT-001",
		"itm_model":        "Part Widget",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	bomID := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "BOM-001",
		"itm_id":         finalItemID,
		"bom_note":       "Initial BOM",
		"bom_status":     "Active",
	})
	_ = createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
		"bom_id":   bomID,
		"itm_id":   partItemID,
		"boc_qty":  3,
		"boc_note": "Part line",
	})

	deleteResp := doAPI(t, client, http.MethodDelete, ts.URL+"/api/tables/boms/"+bomID, token, nil)
	if deleteResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete bom status = %d, want 204", deleteResp.StatusCode)
	}

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/bom_components?limit=20", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("bom_components status = %d, want 200", componentsResp.StatusCode)
	}
	var componentsPayload apiResponse
	decodeJSON(t, componentsResp.Body, &componentsPayload)
	if len(componentsPayload.Rows) != 0 {
		t.Fatalf("expected bom components cascade delete, got %+v", componentsPayload.Rows)
	}

	lastAdminDeleteResp := doAPI(t, client, http.MethodDelete, ts.URL+"/api/tables/users/1", token, nil)
	if lastAdminDeleteResp.StatusCode != http.StatusConflict {
		t.Fatalf("delete last admin status = %d, want 409", lastAdminDeleteResp.StatusCode)
	}
}

func TestSubtableFlows(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"BOM", runBOMSubtableFlowUsesParentContext},
		{"PurchaseOrder", runPurchaseOrderSubtableFlowUsesParentContext},
		{"Quote", runQuoteSubtableFlowUsesParentContext},
		{"SalesOrder", runSalesOrderSubtableFlowUsesParentContext},
		{"Adjustment", runAdjustmentSubtableFlowUsesParentContext},
		{"Invoice", runInvoiceSubtableFlowUsesParentContext},
		{"ManufacturingOrder", runManufacturingOrderSubtableFlowUsesParentContext},
	} {
		t.Run(tc.name, tc.run)
	}
}

func runBOMSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	finalA := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "BOM-FINAL-A",
		"itm_model":        "BOM Final A",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	finalB := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "BOM-FINAL-B",
		"itm_model":        "BOM Final B",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	part := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "BOM-PART-01",
		"itm_model":        "BOM Part",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	bomA := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "BOM-ALPHA",
		"itm_id":         finalA,
		"bom_note":       "Primary alpha BOM",
		"bom_status":     "Active",
	})
	bomB := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "BOM-BETA",
		"itm_id":         finalB,
		"bom_status":     "Active",
	})

	componentA := createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
		"bom_id":   bomA,
		"itm_id":   part,
		"boc_qty":  2,
		"boc_note": "Alpha component",
	})
	_ = createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
		"bom_id":   bomB,
		"itm_id":   part,
		"boc_qty":  4,
		"boc_note": "Beta component",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="bom_components"`) {
		t.Fatalf("dashboard should not expose bom_components in the top nav: %s", dashboardBody)
	}

	bomPanelResp := get(t, client, ts.URL+"/tables/boms?limit=30")
	bomPanelBody := readBody(t, bomPanelResp.Body)
	if bomPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("bom panel status = %d, want 200", bomPanelResp.StatusCode)
	}
	if strings.Contains(bomPanelBody, "Active Table") {
		t.Fatalf("bom panel should not render the legacy active-table eyebrow: %s", bomPanelBody)
	}
	if !strings.Contains(bomPanelBody, `data-child-table="bom_components"`) {
		t.Fatalf("bom panel should advertise its subtable: %s", bomPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/bom_components?limit=30&parent_table=boms&parent_id="+bomA+"&parent_field=bom_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "Alpha component") {
		t.Fatalf("child panel missing filtered component: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, `data-stockit-parent-hat="true"`) {
		t.Fatalf("child panel missing selectable parent hat: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "Beta component") {
		t.Fatalf("child panel should exclude components from other BOMs: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, `StockIt.sortTable('bom_id')`) {
		t.Fatalf("child panel should hide the inherited bom_id column: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "Selected BOM") || !strings.Contains(childPanelBody, ">BOM</span>") || !strings.Contains(childPanelBody, "BOM-ALPHA") {
		t.Fatalf("child panel missing compact BOM context line: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "Primary alpha BOM") || !strings.Contains(childPanelBody, finalA+" | BOM-FINAL-A | BOM Final A") {
		t.Fatalf("child panel hat should show compact BOM field summary: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, part+" | BOM-PART-01 | BOM Part") {
		t.Fatalf("child panel should render item_id with compact item reference details: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, ">Edit<") {
		t.Fatalf("child panel hat should not show the Edit label: %s", childPanelBody)
	}

	bomFormResp := get(t, client, ts.URL+"/tables/boms/form?id="+bomA)
	bomFormBody := readBody(t, bomFormResp.Body)
	if bomFormResp.StatusCode != http.StatusOK {
		t.Fatalf("bom form status = %d, want 200", bomFormResp.StatusCode)
	}
	if !strings.Contains(bomFormBody, `name="bom_status"`) || !strings.Contains(bomFormBody, `<option value="Active" selected>Active</option>`) {
		t.Fatalf("bom form should render status as a selected dropdown: %s", bomFormBody)
	}
	if !strings.Contains(bomFormBody, finalA+` | BOM-FINAL-A | BOM Final A`) {
		t.Fatalf("bom form should show compact item reference labels: %s", bomFormBody)
	}

	formResp := get(t, client, ts.URL+"/tables/bom_components/form?id="+componentA+"&parent_table=boms&parent_id="+bomA+"&parent_field=bom_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("child form status = %d, want 200", formResp.StatusCode)
	}
	if strings.Contains(formBody, `name="bom_id"`) && strings.Contains(formBody, `<select class="stockit-select mt-2 block w-full" name="bom_id"`) {
		t.Fatalf("child form should hide the inherited bom_id selector: %s", formBody)
	}
	if !strings.Contains(formBody, `type="hidden" name="bom_id" value="`+bomA+`"`) {
		t.Fatalf("child form should include hidden bom_id: %s", formBody)
	}
	if !strings.Contains(formBody, `type="hidden" name="parent_table" value="boms"`) {
		t.Fatalf("child form missing parent context: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/bom_components/save", url.Values{
		"parent_table": {"boms"},
		"parent_id":    {bomA},
		"parent_field": {"bom_id"},
		"itm_id":       {part},
		"boc_qty":      {"7"},
		"boc_note":     {"Auto linked component"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/bom_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["boc_note"]) != "Auto linked component" {
			continue
		}
		found = true
		if fmt.Sprint(row["bom_id"]) != bomA {
			t.Fatalf("auto-linked component attached to bom_id=%v, want %s", row["bom_id"], bomA)
		}
	}
	if !found {
		t.Fatalf("auto-linked component not found in API payload: %+v", payload.Rows)
	}
}

func runPurchaseOrderSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	supplierA := createRecord(t, client, token, ts.URL, "suppliers", map[string]any{
		"sup_code":    "SUP-PO-A",
		"sup_name_en": "Supplier PO A",
		"sup_status":  "Active",
	})
	supplierB := createRecord(t, client, token, ts.URL, "suppliers", map[string]any{
		"sup_code":    "SUP-PO-B",
		"sup_name_en": "Supplier PO B",
		"sup_status":  "Active",
	})

	finalA := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "PO-FINAL-A",
		"itm_model":        "PO Final A",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	finalB := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "PO-FINAL-B",
		"itm_model":        "PO Final B",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	componentItem := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "PO-COMP-01",
		"itm_model":        "PO Component",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	porA := createRecord(t, client, token, ts.URL, "purchase_orders", map[string]any{
		"sup_id":         supplierA,
		"por_doc_number": "PO-ALPHA",
		"por_doc_date":   "2026-03-20",
		"itm_id":         finalA,
		"por_ship_date":  "2026-03-22",
		"por_paid_date":  "2026-03-21",
		"por_status":     "approved",
		"por_note":       "Primary alpha PO",
	})
	porB := createRecord(t, client, token, ts.URL, "purchase_orders", map[string]any{
		"sup_id":         supplierB,
		"por_doc_number": "PO-BETA",
		"por_doc_date":   "2026-03-23",
		"itm_id":         finalB,
		"por_status":     "sent",
		"por_note":       "Secondary beta PO",
	})

	componentA := createRecord(t, client, token, ts.URL, "po_components", map[string]any{
		"por_id":             porA,
		"itm_id":             componentItem,
		"poc_qty":            2,
		"poc_price":          3.25,
		"poc_currency":       "USD",
		"poc_shipped_date":   "2026-03-22",
		"poc_delivered_date": "2026-03-24",
		"poc_delivered_qty":  2,
		"poc_received_date":  "2026-03-25",
		"poc_received_qty":   2,
	})
	_ = createRecord(t, client, token, ts.URL, "po_components", map[string]any{
		"por_id":           porB,
		"itm_id":           componentItem,
		"poc_qty":          5,
		"poc_price":        4.1,
		"poc_currency":     "TWD",
		"poc_received_qty": 1,
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="po_components"`) {
		t.Fatalf("dashboard should not expose po_components in the top nav: %s", dashboardBody)
	}

	poPanelResp := get(t, client, ts.URL+"/tables/purchase_orders?limit=30")
	poPanelBody := readBody(t, poPanelResp.Body)
	if poPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("purchase_orders panel status = %d, want 200", poPanelResp.StatusCode)
	}
	if !strings.Contains(poPanelBody, `data-child-table="po_components"`) {
		t.Fatalf("purchase_orders panel should advertise its subtable: %s", poPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/po_components?limit=30&parent_table=purchase_orders&parent_id="+porA+"&parent_field=por_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("po_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "PO-ALPHA") || !strings.Contains(childPanelBody, "Primary alpha PO") {
		t.Fatalf("child panel missing purchase order context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, `data-stockit-parent-hat="true"`) {
		t.Fatalf("child panel missing selectable purchase order hat: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "USD") {
		t.Fatalf("child panel missing alpha component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "TWD") {
		t.Fatalf("child panel should exclude components from other purchase orders: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, `StockIt.sortTable('por_id')`) {
		t.Fatalf("child panel should hide the inherited por_id column: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "Selected Purchase Order") || !strings.Contains(childPanelBody, ">Purchase Order</span>") {
		t.Fatalf("child panel missing compact purchase order context line: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, supplierA+" | SUP-PO-A | Supplier PO A") || !strings.Contains(childPanelBody, finalA+" | PO-FINAL-A | PO Final A") {
		t.Fatalf("child panel should show compact purchase order reference labels: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, componentItem+" | PO-COMP-01 | PO Component") {
		t.Fatalf("child panel should render child item reference details: %s", childPanelBody)
	}

	poFormResp := get(t, client, ts.URL+"/tables/purchase_orders/form?id="+porA)
	poFormBody := readBody(t, poFormResp.Body)
	if poFormResp.StatusCode != http.StatusOK {
		t.Fatalf("purchase_orders form status = %d, want 200", poFormResp.StatusCode)
	}
	if !strings.Contains(poFormBody, `type="date" name="por_doc_date" value="2026-03-20"`) {
		t.Fatalf("purchase order form should render por_doc_date as a date input: %s", poFormBody)
	}
	if !strings.Contains(poFormBody, `<option value="approved" selected>approved</option>`) {
		t.Fatalf("purchase order form should render por_status as a selected dropdown: %s", poFormBody)
	}
	if !strings.Contains(poFormBody, supplierA+` | SUP-PO-A | Supplier PO A`) {
		t.Fatalf("purchase order form should show compact supplier reference labels: %s", poFormBody)
	}

	formResp := get(t, client, ts.URL+"/tables/po_components/form?id="+componentA+"&parent_table=purchase_orders&parent_id="+porA+"&parent_field=por_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("po_components child form status = %d, want 200", formResp.StatusCode)
	}
	if strings.Contains(formBody, `name="por_id"`) && strings.Contains(formBody, `<select class="stockit-select stockit-field-control block w-full" name="por_id"`) {
		t.Fatalf("child form should hide the inherited por_id selector: %s", formBody)
	}
	if !strings.Contains(formBody, `type="hidden" name="por_id" value="`+porA+`"`) {
		t.Fatalf("child form should include hidden por_id: %s", formBody)
	}
	if !strings.Contains(formBody, `type="date" name="poc_shipped_date" value="2026-03-22"`) {
		t.Fatalf("po component form should render shipped date as a date input: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/po_components/save", url.Values{
		"parent_table":      {"purchase_orders"},
		"parent_id":         {porA},
		"parent_field":      {"por_id"},
		"itm_id":            {componentItem},
		"poc_qty":           {"7"},
		"poc_price":         {"5.50"},
		"poc_currency":      {"EUR"},
		"poc_shipped_date":  {"2026-03-26"},
		"poc_delivered_qty": {"7"},
		"poc_received_date": {"2026-03-27"},
		"poc_received_qty":  {"6"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("po_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/po_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("po_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["poc_currency"]) != "EUR" || fmt.Sprint(row["poc_qty"]) != "7" {
			continue
		}
		found = true
		if fmt.Sprint(row["por_id"]) != porA {
			t.Fatalf("auto-linked po component attached to por_id=%v, want %s", row["por_id"], porA)
		}
	}
	if !found {
		t.Fatalf("auto-linked po component not found in API payload: %+v", payload.Rows)
	}
}

func runQuoteSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	supplierA := createRecord(t, client, token, ts.URL, "suppliers", map[string]any{
		"sup_code":    "SUP-QT-A",
		"sup_name_en": "Supplier Quote A",
		"sup_status":  "Active",
	})
	supplierB := createRecord(t, client, token, ts.URL, "suppliers", map[string]any{
		"sup_code":    "SUP-QT-B",
		"sup_name_en": "Supplier Quote B",
		"sup_status":  "Active",
	})

	finalA := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "QT-FINAL-A",
		"itm_model":        "Quote Final A",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	finalB := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "QT-FINAL-B",
		"itm_model":        "Quote Final B",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	componentItem := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "QT-COMP-01",
		"itm_model":        "Quote Component",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	quoteA := createRecord(t, client, token, ts.URL, "quotes", map[string]any{
		"sup_id":         supplierA,
		"qot_doc_number": "QT-ALPHA",
		"qot_doc_date":   "2026-03-20",
		"itm_id":         finalA,
		"qot_status":     "active",
	})
	quoteB := createRecord(t, client, token, ts.URL, "quotes", map[string]any{
		"sup_id":         supplierB,
		"qot_doc_number": "QT-BETA",
		"qot_doc_date":   "2026-03-21",
		"itm_id":         finalB,
		"qot_status":     "inactive",
	})

	componentA := createRecord(t, client, token, ts.URL, "quote_components", map[string]any{
		"qot_id":        quoteA,
		"itm_id":        componentItem,
		"qot_moq":       10,
		"qot_qty":       25,
		"qot_price":     2.75,
		"qot_currency":  "USD",
		"qot_lead_time": "14 days",
	})
	_ = createRecord(t, client, token, ts.URL, "quote_components", map[string]any{
		"qot_id":        quoteB,
		"itm_id":        componentItem,
		"qot_moq":       50,
		"qot_qty":       100,
		"qot_price":     3.2,
		"qot_currency":  "TWD",
		"qot_lead_time": "30 days",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="quote_components"`) {
		t.Fatalf("dashboard should not expose quote_components in the top nav: %s", dashboardBody)
	}

	quotePanelResp := get(t, client, ts.URL+"/tables/quotes?limit=30")
	quotePanelBody := readBody(t, quotePanelResp.Body)
	if quotePanelResp.StatusCode != http.StatusOK {
		t.Fatalf("quotes panel status = %d, want 200", quotePanelResp.StatusCode)
	}
	if !strings.Contains(quotePanelBody, `data-child-table="quote_components"`) {
		t.Fatalf("quotes panel should advertise its subtable: %s", quotePanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/quote_components?limit=30&parent_table=quotes&parent_id="+quoteA+"&parent_field=qot_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("quote_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "QT-ALPHA") {
		t.Fatalf("child panel missing quote context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, `data-stockit-parent-hat="true"`) {
		t.Fatalf("child panel missing selectable quote hat: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "14 days") {
		t.Fatalf("child panel missing alpha quote component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "30 days") {
		t.Fatalf("child panel should exclude components from other quotes: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, `StockIt.sortTable('qot_id')`) {
		t.Fatalf("child panel should hide the inherited qot_id column: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "Selected Quote") || !strings.Contains(childPanelBody, ">Quote</span>") {
		t.Fatalf("child panel missing compact quote context line: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, supplierA+" | SUP-QT-A | Supplier Quote A") || !strings.Contains(childPanelBody, finalA+" | QT-FINAL-A | Quote Final A") {
		t.Fatalf("child panel should show compact quote reference labels: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, componentItem+" | QT-COMP-01 | Quote Component") {
		t.Fatalf("child panel should render child item reference details: %s", childPanelBody)
	}

	quoteFormResp := get(t, client, ts.URL+"/tables/quotes/form?id="+quoteA)
	quoteFormBody := readBody(t, quoteFormResp.Body)
	if quoteFormResp.StatusCode != http.StatusOK {
		t.Fatalf("quotes form status = %d, want 200", quoteFormResp.StatusCode)
	}
	if !strings.Contains(quoteFormBody, `type="date" name="qot_doc_date" value="2026-03-20"`) {
		t.Fatalf("quote form should render qot_doc_date as a date input: %s", quoteFormBody)
	}
	if !strings.Contains(quoteFormBody, `<option value="active" selected>active</option>`) {
		t.Fatalf("quote form should render qot_status as a selected dropdown: %s", quoteFormBody)
	}

	formResp := get(t, client, ts.URL+"/tables/quote_components/form?id="+componentA+"&parent_table=quotes&parent_id="+quoteA+"&parent_field=qot_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("quote_components child form status = %d, want 200", formResp.StatusCode)
	}
	if strings.Contains(formBody, `name="qot_id"`) && strings.Contains(formBody, `<select class="stockit-select stockit-field-control block w-full" name="qot_id"`) {
		t.Fatalf("child form should hide the inherited qot_id selector: %s", formBody)
	}
	if !strings.Contains(formBody, `type="hidden" name="qot_id" value="`+quoteA+`"`) {
		t.Fatalf("child form should include hidden qot_id: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/quote_components/save", url.Values{
		"parent_table":  {"quotes"},
		"parent_id":     {quoteA},
		"parent_field":  {"qot_id"},
		"itm_id":        {componentItem},
		"qot_moq":       {"12"},
		"qot_qty":       {"36"},
		"qot_price":     {"2.95"},
		"qot_currency":  {"EUR"},
		"qot_lead_time": {"21 days"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("quote_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/quote_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("quote_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["qot_currency"]) != "EUR" || fmt.Sprint(row["qot_qty"]) != "36" {
			continue
		}
		found = true
		if fmt.Sprint(row["qot_id"]) != quoteA {
			t.Fatalf("auto-linked quote component attached to qot_id=%v, want %s", row["qot_id"], quoteA)
		}
	}
	if !found {
		t.Fatalf("auto-linked quote component not found in API payload: %+v", payload.Rows)
	}
}

func runSalesOrderSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	customerA := createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Customer SO A",
		"cus_status":  "Active",
	})
	customerB := createRecord(t, client, token, ts.URL, "customers", map[string]any{
		"cus_name_en": "Customer SO B",
		"cus_status":  "Active",
	})
	componentItem := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "SO-COMP-01",
		"itm_model":        "Sales Component",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	orderA := createRecord(t, client, token, ts.URL, "sales_orders", map[string]any{
		"cus_id":         customerA,
		"sor_doc_number": "SO-ALPHA",
		"sor_doc_date":   "2026-03-20",
		"sor_ship_date":  "2026-03-22",
		"sor_paid_date":  "2026-03-23",
		"sor_status":     "confirmed",
	})
	orderB := createRecord(t, client, token, ts.URL, "sales_orders", map[string]any{
		"cus_id":         customerB,
		"sor_doc_number": "SO-BETA",
		"sor_doc_date":   "2026-03-21",
		"sor_status":     "prepared",
	})

	componentA := createRecord(t, client, token, ts.URL, "sales_order_components", map[string]any{
		"sor_id":              orderA,
		"itm_id":              componentItem,
		"sor_qty":             8,
		"sor_price":           11.5,
		"sor_currency":        "USD",
		"sor_ship_date":       "2026-03-22",
		"sor_shipped_date":    "2026-03-24",
		"sor_shipped_qty":     4,
		"sor_shipped_trackno": "TRACK-A",
	})
	_ = createRecord(t, client, token, ts.URL, "sales_order_components", map[string]any{
		"sor_id":              orderB,
		"itm_id":              componentItem,
		"sor_qty":             5,
		"sor_price":           9.8,
		"sor_currency":        "TWD",
		"sor_shipped_trackno": "TRACK-B",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="sales_order_components"`) {
		t.Fatalf("dashboard should not expose sales_order_components in the top nav: %s", dashboardBody)
	}

	orderPanelResp := get(t, client, ts.URL+"/tables/sales_orders?limit=30")
	orderPanelBody := readBody(t, orderPanelResp.Body)
	if orderPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("sales_orders panel status = %d, want 200", orderPanelResp.StatusCode)
	}
	if !strings.Contains(orderPanelBody, `data-child-table="sales_order_components"`) {
		t.Fatalf("sales_orders panel should advertise its subtable: %s", orderPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/sales_order_components?limit=30&parent_table=sales_orders&parent_id="+orderA+"&parent_field=sor_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("sales_order_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "SO-ALPHA") {
		t.Fatalf("child panel missing sales order context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, `data-stockit-parent-hat="true"`) {
		t.Fatalf("child panel missing selectable sales order hat: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "TRACK-A") {
		t.Fatalf("child panel missing alpha sales order component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "TRACK-B") {
		t.Fatalf("child panel should exclude components from other sales orders: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, `StockIt.sortTable('sor_id')`) {
		t.Fatalf("child panel should hide the inherited sor_id column: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "Selected Sales Order") || !strings.Contains(childPanelBody, ">Sales Order</span>") {
		t.Fatalf("child panel missing compact sales order context line: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, customerA+" | Customer SO A") {
		t.Fatalf("child panel should show compact sales order customer label: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, componentItem+" | SO-COMP-01 | Sales Component") {
		t.Fatalf("child panel should render child item reference details: %s", childPanelBody)
	}

	orderFormResp := get(t, client, ts.URL+"/tables/sales_orders/form?id="+orderA)
	orderFormBody := readBody(t, orderFormResp.Body)
	if orderFormResp.StatusCode != http.StatusOK {
		t.Fatalf("sales_orders form status = %d, want 200", orderFormResp.StatusCode)
	}
	if !strings.Contains(orderFormBody, `type="date" name="sor_doc_date" value="2026-03-20"`) {
		t.Fatalf("sales order form should render sor_doc_date as a date input: %s", orderFormBody)
	}
	if !strings.Contains(orderFormBody, `type="date" name="sor_paid_date" value="2026-03-23"`) {
		t.Fatalf("sales order form should render sor_paid_date as a date input: %s", orderFormBody)
	}
	if !strings.Contains(orderFormBody, `<option value="confirmed" selected>confirmed</option>`) {
		t.Fatalf("sales order form should render sor_status as a selected dropdown: %s", orderFormBody)
	}

	formResp := get(t, client, ts.URL+"/tables/sales_order_components/form?id="+componentA+"&parent_table=sales_orders&parent_id="+orderA+"&parent_field=sor_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("sales_order_components child form status = %d, want 200", formResp.StatusCode)
	}
	if strings.Contains(formBody, `name="sor_id"`) && strings.Contains(formBody, `<select class="stockit-select stockit-field-control block w-full" name="sor_id"`) {
		t.Fatalf("child form should hide the inherited sor_id selector: %s", formBody)
	}
	if !strings.Contains(formBody, `type="hidden" name="sor_id" value="`+orderA+`"`) {
		t.Fatalf("child form should include hidden sor_id: %s", formBody)
	}
	if !strings.Contains(formBody, `type="date" name="sor_ship_date" value="2026-03-22"`) {
		t.Fatalf("sales order component form should render ship date as a date input: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/sales_order_components/save", url.Values{
		"parent_table":        {"sales_orders"},
		"parent_id":           {orderA},
		"parent_field":        {"sor_id"},
		"itm_id":              {componentItem},
		"sor_qty":             {"9"},
		"sor_price":           {"12.40"},
		"sor_currency":        {"EUR"},
		"sor_ship_date":       {"2026-03-25"},
		"sor_shipped_date":    {"2026-03-26"},
		"sor_shipped_qty":     {"9"},
		"sor_shipped_trackno": {"TRACK-C"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("sales_order_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/sales_order_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("sales_order_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["sor_shipped_trackno"]) != "TRACK-C" || fmt.Sprint(row["sor_qty"]) != "9" {
			continue
		}
		found = true
		if fmt.Sprint(row["sor_id"]) != orderA {
			t.Fatalf("auto-linked sales order component attached to sor_id=%v, want %s", row["sor_id"], orderA)
		}
	}
	if !found {
		t.Fatalf("auto-linked sales order component not found in API payload: %+v", payload.Rows)
	}
}

func TestStockMovesTopLevelFlow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	item := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "STM-ITM-01",
		"itm_model":        "STM Item",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	loc := createRecord(t, client, token, ts.URL, "locations", map[string]any{
		"loc_name":   "STM Bin",
		"loc_zone":   "storage",
		"loc_status": "Active",
	})
	por := createRecord(t, client, token, ts.URL, "purchase_orders", map[string]any{
		"por_doc_number": "STM-PO-01",
		"por_doc_date":   "2026-04-08",
		"por_status":     "received",
	})

	stmID := createRecord(t, client, token, ts.URL, "stock_moves", map[string]any{
		"stm_doc_number": "STM-RECEIPT-01",
		"stm_date":       "2026-04-09",
		"por_id":         por,
		"itm_id":         item,
		"stm_dst_loc_id": loc,
		"stm_qty":        7,
		"stm_note":       "STM-ALPHA-NOTE",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if !strings.Contains(dashboardBody, `data-table="stock_moves"`) {
		t.Fatalf("dashboard should expose stock_moves as a top-level table: %s", dashboardBody)
	}

	panelResp := get(t, client, ts.URL+"/tables/stock_moves?limit=30")
	panelBody := readBody(t, panelResp.Body)
	if panelResp.StatusCode != http.StatusOK {
		t.Fatalf("stock_moves panel status = %d, want 200", panelResp.StatusCode)
	}
	if !strings.Contains(panelBody, "STM-RECEIPT-01") {
		t.Fatalf("stock_moves panel missing STM-RECEIPT-01: %s", panelBody)
	}
	if !strings.Contains(panelBody, "STM-ALPHA-NOTE") {
		t.Fatalf("stock_moves panel missing note: %s", panelBody)
	}

	formResp := get(t, client, ts.URL+"/tables/stock_moves/form?id="+stmID)
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("stock_moves form status = %d, want 200", formResp.StatusCode)
	}
	if !strings.Contains(formBody, `name="stm_doc_number"`) {
		t.Fatalf("stock_moves form should render stm_doc_number input: %s", formBody)
	}
	if !strings.Contains(formBody, `name="por_id"`) {
		t.Fatalf("stock_moves form should render por_id select: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/stock_moves/save", url.Values{
		"id":             {stmID},
		"stm_doc_number": {"STM-RECEIPT-01"},
		"stm_date":       {"2026-04-09"},
		"por_id":         {por},
		"itm_id":         {item},
		"stm_dst_loc_id": {loc},
		"stm_qty":        {"7"},
		"stm_note":       {"STM-UPDATED"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("stock_moves save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	apiResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/stock_moves?limit=30", token, nil)
	if apiResp.StatusCode != http.StatusOK {
		t.Fatalf("stock_moves api status = %d, want 200", apiResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, apiResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["stm_note"]) != "STM-UPDATED" {
			continue
		}
		found = true
		if fmt.Sprint(row["por_id"]) != por {
			t.Fatalf("stock_moves row por_id = %v, want %s", row["por_id"], por)
		}
	}
	if !found {
		t.Fatalf("updated stock_moves row not found in API payload: %+v", payload.Rows)
	}

	initResp := doMCP(t, client, ts.URL+"/mcp", token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
	})
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	_ = initResp.Body.Close()
	callResp := doMCP(t, client, ts.URL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcpToolListTables, "arguments": map[string]any{}},
	})
	body := readBody(t, callResp.Body)
	if !strings.Contains(body, "stock_moves") {
		t.Fatalf("mcp list_tables missing stock_moves: %s", body)
	}

	// Verify src == dst location is rejected.
	sameLocResp := postForm(t, client, ts.URL+"/tables/stock_moves/save", url.Values{
		"stm_doc_number": {"STM-BAD"},
		"stm_date":       {"2026-04-09"},
		"itm_id":         {item},
		"stm_src_loc_id": {loc},
		"stm_dst_loc_id": {loc},
		"stm_qty":        {"1"},
	})
	sameLocBody := readBody(t, sameLocResp.Body)
	if sameLocResp.StatusCode == http.StatusNoContent {
		t.Fatalf("stock_moves should reject same src and dst location")
	}
	if !strings.Contains(sameLocBody, "Source and destination locations must be different") {
		t.Fatalf("expected validation error for same locations, got: %s", sameLocBody)
	}
}

func runAdjustmentSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	item := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "ADJ-ITM-01",
		"itm_model":        "ADJ Item",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	loc := createRecord(t, client, token, ts.URL, "locations", map[string]any{
		"loc_name":   "ADJ Bin",
		"loc_zone":   "storage",
		"loc_status": "Active",
	})

	adjA := createRecord(t, client, token, ts.URL, "adjustments", map[string]any{
		"adj_doc_number": "ADJ-ALPHA",
		"adj_doc_date":   "2026-04-08",
		"adj_reason":     "cycle_count",
		"adj_note":       "alpha",
	})
	adjB := createRecord(t, client, token, ts.URL, "adjustments", map[string]any{
		"adj_doc_number": "ADJ-BETA",
		"adj_doc_date":   "2026-04-09",
		"adj_reason":     "damage",
	})

	componentA := createRecord(t, client, token, ts.URL, "adjustment_components", map[string]any{
		"adj_id":   adjA,
		"itm_id":   item,
		"loc_id":   loc,
		"adc_qty":  5,
		"adc_note": "ADC-ALPHA-NOTE",
	})
	_ = createRecord(t, client, token, ts.URL, "adjustment_components", map[string]any{
		"adj_id":   adjB,
		"itm_id":   item,
		"loc_id":   loc,
		"adc_qty":  -2,
		"adc_note": "ADC-BETA-NOTE",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="adjustment_components"`) {
		t.Fatalf("dashboard should not expose adjustment_components in the top nav: %s", dashboardBody)
	}

	adjPanelResp := get(t, client, ts.URL+"/tables/adjustments?limit=30")
	adjPanelBody := readBody(t, adjPanelResp.Body)
	if adjPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("adjustments panel status = %d, want 200", adjPanelResp.StatusCode)
	}
	if !strings.Contains(adjPanelBody, `data-child-table="adjustment_components"`) {
		t.Fatalf("adjustments panel should advertise its subtable: %s", adjPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/adjustment_components?limit=30&parent_table=adjustments&parent_id="+adjA+"&parent_field=adj_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("adjustment_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "ADJ-ALPHA") {
		t.Fatalf("child panel missing adjustment context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "ADC-ALPHA-NOTE") {
		t.Fatalf("child panel missing alpha component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "ADC-BETA-NOTE") {
		t.Fatalf("child panel should exclude components from other adjustments: %s", childPanelBody)
	}

	formResp := get(t, client, ts.URL+"/tables/adjustment_components/form?id="+componentA+"&parent_table=adjustments&parent_id="+adjA+"&parent_field=adj_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("adjustment_components child form status = %d, want 200", formResp.StatusCode)
	}
	if !strings.Contains(formBody, `type="hidden" name="adj_id" value="`+adjA+`"`) {
		t.Fatalf("child form should include hidden adj_id: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/adjustment_components/save", url.Values{
		"parent_table": {"adjustments"},
		"parent_id":    {adjA},
		"parent_field": {"adj_id"},
		"itm_id":       {item},
		"loc_id":       {loc},
		"adc_qty":      {"-3"},
		"adc_note":     {"ADC-NEW"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("adjustment_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/adjustment_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("adjustment_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["adc_note"]) != "ADC-NEW" {
			continue
		}
		found = true
		if fmt.Sprint(row["adj_id"]) != adjA {
			t.Fatalf("auto-linked adjustment component attached to adj_id=%v, want %s", row["adj_id"], adjA)
		}
	}
	if !found {
		t.Fatalf("auto-linked adjustment component not found in API payload: %+v", payload.Rows)
	}

	initResp := doMCP(t, client, ts.URL+"/mcp", token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
	})
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	_ = initResp.Body.Close()
	callResp := doMCP(t, client, ts.URL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcpToolListTables, "arguments": map[string]any{}},
	})
	body := readBody(t, callResp.Body)
	if !strings.Contains(body, "adjustments") {
		t.Fatalf("mcp list_tables missing adjustments: %s", body)
	}
}

func runInvoiceSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	finalItem := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "INV-FINAL-01",
		"itm_model":        "INV Final",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	invA := createRecord(t, client, token, ts.URL, "invoices", map[string]any{
		"inv_doc_number": "INV-ALPHA",
		"inv_doc_date":   "2026-04-01",
		"inv_shipped_by": "FedEx",
	})
	invB := createRecord(t, client, token, ts.URL, "invoices", map[string]any{
		"inv_doc_number": "INV-BETA",
		"inv_doc_date":   "2026-04-02",
		"inv_shipped_by": "DHL",
	})

	componentA := createRecord(t, client, token, ts.URL, "invoice_components", map[string]any{
		"inv_id":       invA,
		"itm_id":       finalItem,
		"ivc_qty":      7,
		"ivc_price":    123.45,
		"ivc_currency": "USD",
	})
	_ = createRecord(t, client, token, ts.URL, "invoice_components", map[string]any{
		"inv_id":       invB,
		"itm_id":       finalItem,
		"ivc_qty":      3,
		"ivc_price":    50,
		"ivc_currency": "EUR",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="invoice_components"`) {
		t.Fatalf("dashboard should not expose invoice_components in the top nav: %s", dashboardBody)
	}

	invPanelResp := get(t, client, ts.URL+"/tables/invoices?limit=30")
	invPanelBody := readBody(t, invPanelResp.Body)
	if invPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("invoices panel status = %d, want 200", invPanelResp.StatusCode)
	}
	if !strings.Contains(invPanelBody, `data-child-table="invoice_components"`) {
		t.Fatalf("invoices panel should advertise its subtable: %s", invPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/invoice_components?limit=30&parent_table=invoices&parent_id="+invA+"&parent_field=inv_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("invoice_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "INV-ALPHA") {
		t.Fatalf("child panel missing invoice context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "USD") {
		t.Fatalf("child panel missing alpha invoice component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "EUR") {
		t.Fatalf("child panel should exclude components from other invoices: %s", childPanelBody)
	}

	formResp := get(t, client, ts.URL+"/tables/invoice_components/form?id="+componentA+"&parent_table=invoices&parent_id="+invA+"&parent_field=inv_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("invoice_components child form status = %d, want 200", formResp.StatusCode)
	}
	if !strings.Contains(formBody, `type="hidden" name="inv_id" value="`+invA+`"`) {
		t.Fatalf("child form should include hidden inv_id: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/invoice_components/save", url.Values{
		"parent_table": {"invoices"},
		"parent_id":    {invA},
		"parent_field": {"inv_id"},
		"itm_id":       {finalItem},
		"ivc_qty":      {"11"},
		"ivc_price":    {"99.5"},
		"ivc_currency": {"TWD"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("invoice_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/invoice_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("invoice_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["ivc_currency"]) != "TWD" {
			continue
		}
		found = true
		if fmt.Sprint(row["inv_id"]) != invA {
			t.Fatalf("auto-linked invoice component attached to inv_id=%v, want %s", row["inv_id"], invA)
		}
	}
	if !found {
		t.Fatalf("auto-linked invoice component not found in API payload: %+v", payload.Rows)
	}

	initResp := doMCP(t, client, ts.URL+"/mcp", token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
	})
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	_ = initResp.Body.Close()
	callResp := doMCP(t, client, ts.URL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcpToolListTables, "arguments": map[string]any{}},
	})
	body := readBody(t, callResp.Body)
	if !strings.Contains(body, "invoices") {
		t.Fatalf("mcp list_tables missing invoices: %s", body)
	}
}

func runManufacturingOrderSubtableFlowUsesParentContext(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	finalItem := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "MFO-FINAL-01",
		"itm_model":        "MFO Final",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})

	mfoA := createRecord(t, client, token, ts.URL, "manufacturing_orders", map[string]any{
		"mfo_doc_number":  "MFO-ALPHA",
		"mfo_doc_date":    "2026-04-01",
		"mfo_target_date": "2026-04-15",
	})
	mfoB := createRecord(t, client, token, ts.URL, "manufacturing_orders", map[string]any{
		"mfo_doc_number":  "MFO-BETA",
		"mfo_doc_date":    "2026-04-02",
		"mfo_target_date": "2026-04-20",
	})

	componentA := createRecord(t, client, token, ts.URL, "mfo_components", map[string]any{
		"mfo_id":        mfoA,
		"itm_id":        finalItem,
		"mfc_qty":       7,
		"mfc_qc_date":   "2026-04-10",
		"mfc_fqc_date":  "2026-04-12",
		"mfc_pack_date": "2026-04-13",
		"mfc_note":      "MFC-ALPHA-NOTE",
	})
	_ = createRecord(t, client, token, ts.URL, "mfo_components", map[string]any{
		"mfo_id":   mfoB,
		"itm_id":   finalItem,
		"mfc_qty":  3,
		"mfc_note": "MFC-BETA-NOTE",
	})

	dashboardResp := get(t, client, ts.URL+"/")
	dashboardBody := readBody(t, dashboardResp.Body)
	if dashboardResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashboardResp.StatusCode)
	}
	if strings.Contains(dashboardBody, `data-table="mfo_components"`) {
		t.Fatalf("dashboard should not expose mfo_components in the top nav: %s", dashboardBody)
	}

	mfoPanelResp := get(t, client, ts.URL+"/tables/manufacturing_orders?limit=30")
	mfoPanelBody := readBody(t, mfoPanelResp.Body)
	if mfoPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("manufacturing_orders panel status = %d, want 200", mfoPanelResp.StatusCode)
	}
	if !strings.Contains(mfoPanelBody, `data-child-table="mfo_components"`) {
		t.Fatalf("manufacturing_orders panel should advertise its subtable: %s", mfoPanelBody)
	}

	childPanelResp := get(t, client, ts.URL+"/tables/mfo_components?limit=30&parent_table=manufacturing_orders&parent_id="+mfoA+"&parent_field=mfo_id")
	childPanelBody := readBody(t, childPanelResp.Body)
	if childPanelResp.StatusCode != http.StatusOK {
		t.Fatalf("mfo_components child panel status = %d, want 200", childPanelResp.StatusCode)
	}
	if !strings.Contains(childPanelBody, "MFO-ALPHA") {
		t.Fatalf("child panel missing manufacturing order context: %s", childPanelBody)
	}
	if !strings.Contains(childPanelBody, "MFC-ALPHA-NOTE") {
		t.Fatalf("child panel missing alpha mfo component row: %s", childPanelBody)
	}
	if strings.Contains(childPanelBody, "MFC-BETA-NOTE") {
		t.Fatalf("child panel should exclude components from other manufacturing orders: %s", childPanelBody)
	}

	formResp := get(t, client, ts.URL+"/tables/mfo_components/form?id="+componentA+"&parent_table=manufacturing_orders&parent_id="+mfoA+"&parent_field=mfo_id")
	formBody := readBody(t, formResp.Body)
	if formResp.StatusCode != http.StatusOK {
		t.Fatalf("mfo_components child form status = %d, want 200", formResp.StatusCode)
	}
	if !strings.Contains(formBody, `type="hidden" name="mfo_id" value="`+mfoA+`"`) {
		t.Fatalf("child form should include hidden mfo_id: %s", formBody)
	}
	if !strings.Contains(formBody, `type="date" name="mfc_qc_date" value="2026-04-10"`) {
		t.Fatalf("mfo component form should render qc_date as a date input: %s", formBody)
	}

	saveResp := postForm(t, client, ts.URL+"/tables/mfo_components/save", url.Values{
		"parent_table":  {"manufacturing_orders"},
		"parent_id":     {mfoA},
		"parent_field":  {"mfo_id"},
		"itm_id":        {finalItem},
		"mfc_qty":       {"11"},
		"mfc_qc_date":   {"2026-04-11"},
		"mfc_fqc_date":  {"2026-04-13"},
		"mfc_pack_date": {"2026-04-14"},
		"mfc_note":      {"MFC-NEW"},
	})
	if saveResp.StatusCode != http.StatusNoContent {
		t.Fatalf("mfo_components child save status = %d, want 204", saveResp.StatusCode)
	}
	_ = saveResp.Body.Close()

	componentsResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/mfo_components?limit=30", token, nil)
	if componentsResp.StatusCode != http.StatusOK {
		t.Fatalf("mfo_components api status = %d, want 200", componentsResp.StatusCode)
	}
	var payload apiResponse
	decodeJSON(t, componentsResp.Body, &payload)

	found := false
	for _, row := range payload.Rows {
		if fmt.Sprint(row["mfc_note"]) != "MFC-NEW" {
			continue
		}
		found = true
		if fmt.Sprint(row["mfo_id"]) != mfoA {
			t.Fatalf("auto-linked mfo component attached to mfo_id=%v, want %s", row["mfo_id"], mfoA)
		}
	}
	if !found {
		t.Fatalf("auto-linked mfo component not found in API payload: %+v", payload.Rows)
	}

	// Verify MCP tool exposes the new tables.
	initResp := doMCP(t, client, ts.URL+"/mcp", token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test", "version": "1"}},
	})
	sessionID := initResp.Header.Get("Mcp-Session-Id")
	_ = initResp.Body.Close()
	callResp := doMCP(t, client, ts.URL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]any{"name": mcpToolListTables, "arguments": map[string]any{}},
	})
	body := readBody(t, callResp.Body)
	if !strings.Contains(body, "manufacturing_orders") {
		t.Fatalf("mcp list_tables missing manufacturing_orders: %s", body)
	}
}

func TestDeleteParentFromSubtableContextEmitsRecordDeletedTrigger(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	finalItemID := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "DEL-FG-001",
		"itm_model":        "Delete Flow Final",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	bomID := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "DEL-BOM-001",
		"itm_id":         finalItemID,
		"bom_status":     "Active",
	})

	req, err := http.NewRequest(http.MethodDelete, ts.URL+"/tables/boms/row/"+bomID, nil)
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	req.Header.Set("HX-Request", "true")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("delete request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	trigger := resp.Header.Get("HX-Trigger")
	if !strings.Contains(trigger, `"stockit:record-deleted":{"table":"boms","id":"`+bomID+`"}`) {
		t.Fatalf("delete trigger missing record-deleted payload: %s", trigger)
	}
	if strings.Contains(trigger, `"stockit:refresh-table"`) {
		t.Fatalf("delete trigger should not request a generic table refresh: %s", trigger)
	}
}

func TestSeedReviewDataset(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	customerIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{
			"cus_name_en":        "Review Customer A",
			"cus_name_zh":        "審查客戶甲",
			"cus_phone":          "+886-2-5555-1000",
			"cus_contact_name":   "Nina Lin",
			"cust_contact_email": "nina@example.com",
			"cus_status":         "Active",
		},
		{
			"cus_name_en":        "Review Customer B",
			"cus_name_zh":        "審查客戶乙",
			"cus_phone":          "+886-2-5555-1001",
			"cus_contact_name":   "Owen Lee",
			"cust_contact_email": "owen@example.com",
			"cus_status":         "Under Review",
		},
		{
			"cus_name_en":        "Review Customer C",
			"cus_name_zh":        "審查客戶丙",
			"cus_phone":          "+886-2-5555-1002",
			"cus_contact_name":   "Mia Chen",
			"cust_contact_email": "mia@example.com",
			"cus_status":         "Hold",
		},
	} {
		customerIDs = append(customerIDs, createRecord(t, client, token, ts.URL, "customers", payload))
	}

	supplierIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{
			"sup_code":          "SUP-001",
			"sup_name_en":       "Review Supplier A",
			"sup_contact_name":  "Jason Wu",
			"sup_contact_phone": "+886-2-5555-2000",
			"sup_contact_email": "jason@example.com",
			"sup_status":        "Active",
		},
		{
			"sup_code":          "SUP-002",
			"sup_name_en":       "Review Supplier B",
			"sup_contact_name":  "Iris Tsai",
			"sup_contact_phone": "+886-2-5555-2001",
			"sup_contact_email": "iris@example.com",
			"sup_status":        "Under Review",
		},
		{
			"sup_code":          "SUP-003",
			"sup_name_en":       "Review Supplier C",
			"sup_contact_name":  "Alan Hsu",
			"sup_contact_phone": "+886-2-5555-2002",
			"sup_contact_email": "alan@example.com",
			"sup_status":        "Active",
		},
	} {
		supplierIDs = append(supplierIDs, createRecord(t, client, token, ts.URL, "suppliers", payload))
	}

	locationIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"loc_name": "Main Warehouse", "loc_zone": "storage", "loc_status": "Active"},
		{"loc_name": "Assembly Floor", "loc_zone": "assembly", "loc_status": "Active"},
		{"loc_name": "Returns Cage", "loc_zone": "returns", "loc_status": "Hold"},
	} {
		locationIDs = append(locationIDs, createRecord(t, client, token, ts.URL, "locations", payload))
	}

	itemIDs := make([]string, 0, 15)
	for _, payload := range []map[string]any{
		{
			"itm_sku":          "RV-FG-01",
			"itm_model":        "Review Final A",
			"itm_description":  "Finished good for review A",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        125.5,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-FG-02",
			"itm_model":        "Review Final B",
			"itm_description":  "Finished good for review B",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        140.0,
			"itm_status":       "Under Review",
		},
		{
			"itm_sku":          "RV-FG-03",
			"itm_model":        "Review Final C",
			"itm_description":  "Finished good for review C",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        152.0,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-01",
			"itm_model":        "Review Part Alpha",
			"itm_description":  "Component for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        10.25,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-02",
			"itm_model":        "Review Part Beta",
			"itm_description":  "Precision bracket for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        8.5,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-03",
			"itm_model":        "Review Part Gamma",
			"itm_description":  "Cable assembly for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        6.8,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-04",
			"itm_model":        "Review Part Delta",
			"itm_description":  "Fastener kit for review",
			"itm_type":         "part",
			"itm_measure_unit": "set",
			"itm_value":        3.45,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-05",
			"itm_model":        "Review Part Epsilon",
			"itm_description":  "Sensor module for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        18.25,
			"itm_status":       "Under Review",
		},
		{
			"itm_sku":          "RV-PT-06",
			"itm_model":        "Review Part Zeta",
			"itm_description":  "Motor mount for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        7.9,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-07",
			"itm_model":        "Review Part Eta",
			"itm_description":  "Control knob for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        2.75,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-08",
			"itm_model":        "Review Part Theta",
			"itm_description":  "Display bezel for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        4.4,
			"itm_status":       "Hold",
		},
		{
			"itm_sku":          "RV-PT-09",
			"itm_model":        "Review Part Iota",
			"itm_description":  "Packaging insert for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        1.1,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-AS-01",
			"itm_model":        "Review Assembly Alpha",
			"itm_description":  "Assembly fixture alpha",
			"itm_type":         "assembly",
			"itm_measure_unit": "set",
			"itm_value":        42.0,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-AS-02",
			"itm_model":        "Review Assembly Beta",
			"itm_description":  "Assembly fixture beta",
			"itm_type":         "assembly",
			"itm_measure_unit": "set",
			"itm_value":        45.5,
			"itm_status":       "Under Review",
		},
		{
			"itm_sku":          "RV-AS-03",
			"itm_model":        "Review Assembly Gamma",
			"itm_description":  "Assembly fixture gamma",
			"itm_type":         "assembly",
			"itm_measure_unit": "set",
			"itm_value":        47.75,
			"itm_status":       "Active",
		},
	} {
		itemIDs = append(itemIDs, createRecord(t, client, token, ts.URL, "items", payload))
	}
	finalItemIDs := itemIDs[:3]
	componentItemIDs := itemIDs[3:12]

	bomIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"bom_doc_number": "RV-BOM-01", "itm_id": finalItemIDs[0], "bom_note": "Review BOM A", "bom_status": "Under Review"},
		{"bom_doc_number": "RV-BOM-02", "itm_id": finalItemIDs[1], "bom_note": "Review BOM B", "bom_status": "Active"},
		{"bom_doc_number": "RV-BOM-03", "itm_id": finalItemIDs[2], "bom_note": "Review BOM C", "bom_status": "Draft"},
	} {
		bomIDs = append(bomIDs, createRecord(t, client, token, ts.URL, "boms", payload))
	}

	for bomIndex, bomID := range bomIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
				"bom_id":   bomID,
				"itm_id":   componentItemIDs[(bomIndex*3)+lineIndex],
				"boc_qty":  float64((bomIndex + 2) * (lineIndex + 1)),
				"boc_note": fmt.Sprintf("Review BOM component %d%c", bomIndex+1, 'A'+lineIndex),
			})
		}
	}

	porIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{
			"sup_id":         supplierIDs[0],
			"por_doc_number": "RV-PO-01",
			"por_doc_date":   "2026-03-18",
			"itm_id":         finalItemIDs[0],
			"por_ship_date":  "2026-03-21",
			"por_paid_date":  "2026-03-20",
			"por_status":     "approved",
			"por_note":       "Review PO A",
		},
		{
			"sup_id":         supplierIDs[1],
			"por_doc_number": "RV-PO-02",
			"por_doc_date":   "2026-03-19",
			"itm_id":         finalItemIDs[1],
			"por_status":     "sent",
			"por_note":       "Review PO B",
		},
		{
			"sup_id":         supplierIDs[2],
			"por_doc_number": "RV-PO-03",
			"por_doc_date":   "2026-03-20",
			"itm_id":         finalItemIDs[2],
			"por_status":     "received",
			"por_note":       "Review PO C",
		},
	} {
		porIDs = append(porIDs, createRecord(t, client, token, ts.URL, "purchase_orders", payload))
	}

	for orderIndex, porID := range porIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "po_components", map[string]any{
				"por_id":             porID,
				"itm_id":             componentItemIDs[(orderIndex*3)+lineIndex],
				"poc_qty":            float64(5 + orderIndex + lineIndex),
				"poc_price":          2.5 + float64(orderIndex) + (0.35 * float64(lineIndex)),
				"poc_currency":       []string{"USD", "TWD", "EUR"}[lineIndex],
				"poc_shipped_date":   fmt.Sprintf("2026-03-%02d", 21+(orderIndex*3)+lineIndex),
				"poc_delivered_date": fmt.Sprintf("2026-03-%02d", 22+(orderIndex*3)+lineIndex),
				"poc_delivered_qty":  float64(4 + orderIndex + lineIndex),
				"poc_received_date":  fmt.Sprintf("2026-03-%02d", 23+(orderIndex*3)+lineIndex),
				"poc_received_qty":   float64(4 + orderIndex + lineIndex),
			})
		}
	}

	quoteIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"sup_id": supplierIDs[0], "qot_doc_number": "RV-QT-01", "qot_doc_date": "2026-03-17", "itm_id": finalItemIDs[0], "qot_status": "active"},
		{"sup_id": supplierIDs[1], "qot_doc_number": "RV-QT-02", "qot_doc_date": "2026-03-18", "itm_id": finalItemIDs[1], "qot_status": "inactive"},
		{"sup_id": supplierIDs[2], "qot_doc_number": "RV-QT-03", "qot_doc_date": "2026-03-19", "itm_id": finalItemIDs[2], "qot_status": "active"},
	} {
		quoteIDs = append(quoteIDs, createRecord(t, client, token, ts.URL, "quotes", payload))
	}

	for quoteIndex, quoteID := range quoteIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "quote_components", map[string]any{
				"qot_id":        quoteID,
				"itm_id":        componentItemIDs[(quoteIndex*3)+lineIndex],
				"qot_moq":       float64(10 + (quoteIndex * 5) + lineIndex),
				"qot_qty":       float64(25 + (quoteIndex * 10) + (lineIndex * 5)),
				"qot_price":     2.2 + float64(quoteIndex) + (0.2 * float64(lineIndex)),
				"qot_currency":  []string{"USD", "TWD", "EUR"}[lineIndex],
				"qot_lead_time": fmt.Sprintf("Review lead %d%c", quoteIndex+1, 'A'+lineIndex),
			})
		}
	}

	salesOrderIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"cus_id": customerIDs[0], "sor_doc_number": "RV-SO-01", "sor_doc_date": "2026-03-20", "sor_ship_date": "2026-03-22", "sor_paid_date": "2026-03-23", "sor_status": "confirmed"},
		{"cus_id": customerIDs[1], "sor_doc_number": "RV-SO-02", "sor_doc_date": "2026-03-21", "sor_status": "prepared"},
		{"cus_id": customerIDs[2], "sor_doc_number": "RV-SO-03", "sor_doc_date": "2026-03-22", "sor_status": "paid"},
	} {
		salesOrderIDs = append(salesOrderIDs, createRecord(t, client, token, ts.URL, "sales_orders", payload))
	}

	for salesIndex, salesOrderID := range salesOrderIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "sales_order_components", map[string]any{
				"sor_id":              salesOrderID,
				"itm_id":              componentItemIDs[(salesIndex*3)+lineIndex],
				"sor_qty":             float64(4 + salesIndex + lineIndex),
				"sor_price":           9.5 + float64(salesIndex) + (0.4 * float64(lineIndex)),
				"sor_currency":        []string{"USD", "TWD", "EUR"}[lineIndex],
				"sor_ship_date":       fmt.Sprintf("2026-03-%02d", 22+(salesIndex*3)+lineIndex),
				"sor_shipped_date":    fmt.Sprintf("2026-03-%02d", 23+(salesIndex*3)+lineIndex),
				"sor_shipped_qty":     float64(2 + salesIndex + lineIndex),
				"sor_shipped_trackno": fmt.Sprintf("RV-SO-TRACK-%d%c", salesIndex+1, 'A'+lineIndex),
			})
		}
	}

	mfoIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"mfo_doc_number": "RV-MFO-01", "mfo_doc_date": "2026-04-01", "mfo_target_date": "2026-04-15"},
		{"mfo_doc_number": "RV-MFO-02", "mfo_doc_date": "2026-04-02", "mfo_target_date": "2026-04-18"},
		{"mfo_doc_number": "RV-MFO-03", "mfo_doc_date": "2026-04-03", "mfo_target_date": "2026-04-22"},
	} {
		mfoIDs = append(mfoIDs, createRecord(t, client, token, ts.URL, "manufacturing_orders", payload))
	}

	for mfoIndex, mfoID := range mfoIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "mfo_components", map[string]any{
				"mfo_id":        mfoID,
				"itm_id":        finalItemIDs[mfoIndex],
				"bom_id":        bomIDs[mfoIndex],
				"mfc_qty":       float64(5 + mfoIndex + lineIndex),
				"sor_id":        salesOrderIDs[mfoIndex],
				"mfc_qc_date":   fmt.Sprintf("2026-04-%02d", 10+(mfoIndex*3)+lineIndex),
				"mfc_fqc_date":  fmt.Sprintf("2026-04-%02d", 12+(mfoIndex*3)+lineIndex),
				"mfc_pack_date": fmt.Sprintf("2026-04-%02d", 13+(mfoIndex*3)+lineIndex),
				"mfc_note":      fmt.Sprintf("RV-MFC-%d%c", mfoIndex+1, 'A'+lineIndex),
			})
		}
	}

	invoiceIDs := make([]string, 0, 3)
	for invIndex, payload := range []map[string]any{
		{"inv_doc_number": "RV-INV-01", "inv_doc_date": "2026-04-05", "sup_id": supplierIDs[0], "cus_id": customerIDs[0], "sor_id": salesOrderIDs[0], "inv_shipped_by": "FedEx"},
		{"inv_doc_number": "RV-INV-02", "inv_doc_date": "2026-04-06", "sup_id": supplierIDs[1], "cus_id": customerIDs[1], "sor_id": salesOrderIDs[1], "inv_shipped_by": "DHL"},
		{"inv_doc_number": "RV-INV-03", "inv_doc_date": "2026-04-07", "sup_id": supplierIDs[2], "cus_id": customerIDs[2], "sor_id": salesOrderIDs[2], "inv_shipped_by": "UPS"},
	} {
		_ = invIndex
		invoiceIDs = append(invoiceIDs, createRecord(t, client, token, ts.URL, "invoices", payload))
	}

	currencies := []string{"USD", "TWD", "CNY"}
	for invIndex, invID := range invoiceIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "invoice_components", map[string]any{
				"inv_id":       invID,
				"itm_id":       finalItemIDs[invIndex],
				"ivc_qty":      float64(2 + invIndex + lineIndex),
				"ivc_price":    float64(100 + invIndex*10 + lineIndex),
				"ivc_currency": currencies[lineIndex%len(currencies)],
			})
		}
	}

	adjIDs := make([]string, 0, 3)
	adjReasons := []string{"cycle_count", "damage", "found"}
	for adjIndex, reason := range adjReasons {
		adjIDs = append(adjIDs, createRecord(t, client, token, ts.URL, "adjustments", map[string]any{
			"adj_doc_number": fmt.Sprintf("RV-ADJ-%02d", adjIndex+1),
			"adj_doc_date":   fmt.Sprintf("2026-04-%02d", 8+adjIndex),
			"adj_reason":     reason,
			"adj_note":       fmt.Sprintf("RV-ADJ-NOTE-%d", adjIndex+1),
		}))
	}

	for adjIndex, adjID := range adjIDs {
		for lineIndex := range 3 {
			createRecord(t, client, token, ts.URL, "adjustment_components", map[string]any{
				"adj_id":   adjID,
				"itm_id":   finalItemIDs[adjIndex],
				"loc_id":   locationIDs[lineIndex%len(locationIDs)],
				"adc_qty":  float64(lineIndex+1) - float64(adjIndex),
				"adc_note": fmt.Sprintf("RV-ADC-%d%c", adjIndex+1, 'A'+lineIndex),
			})
		}
	}

	createRecord(t, client, token, ts.URL, "stock_moves", map[string]any{
		"stm_doc_number": "RV-STM-01",
		"stm_date":       "2026-04-10",
		"por_id":         porIDs[0],
		"itm_id":         finalItemIDs[0],
		"stm_dst_loc_id": locationIDs[0],
		"stm_qty":        5,
		"stm_note":       "RV-STM-NOTE-RECEIPT",
	})
	createRecord(t, client, token, ts.URL, "stock_moves", map[string]any{
		"stm_doc_number": "RV-STM-02",
		"stm_date":       "2026-04-11",
		"sor_id":         salesOrderIDs[0],
		"itm_id":         finalItemIDs[0],
		"stm_src_loc_id": locationIDs[0],
		"stm_qty":        2,
		"stm_note":       "RV-STM-NOTE-ISSUE",
	})
	createRecord(t, client, token, ts.URL, "stock_moves", map[string]any{
		"stm_doc_number": "RV-STM-03",
		"stm_date":       "2026-04-12",
		"adj_id":         adjIDs[0],
		"itm_id":         finalItemIDs[0],
		"stm_src_loc_id": locationIDs[0],
		"stm_dst_loc_id": locationIDs[1],
		"stm_qty":        1,
		"stm_note":       "RV-STM-NOTE-TRANSFER",
	})

	for _, tc := range []struct {
		table   string
		check   string
		minRows int
	}{
		{table: "manufacturing_orders", check: "RV-MFO-01", minRows: 3},
		{table: "mfo_components", check: "RV-MFC-1A", minRows: 9},
		{table: "invoices", check: "RV-INV-01", minRows: 3},
		{table: "invoice_components", check: "USD", minRows: 9},
		{table: "adjustments", check: "RV-ADJ-01", minRows: 3},
		{table: "adjustment_components", check: "RV-ADC-1A", minRows: 9},
		{table: "stock_moves", check: "RV-STM-01", minRows: 3},
		{table: "users", check: "admin", minRows: 3},
		{table: "customers", check: "Review Customer A", minRows: 3},
		{table: "suppliers", check: "Review Supplier A", minRows: 3},
		{table: "locations", check: "Main Warehouse", minRows: 3},
		{table: "items", check: "RV-FG-01", minRows: 15},
		{table: "boms", check: "RV-BOM-01", minRows: 3},
		{table: "bom_components", check: "Review BOM component 1A", minRows: 9},
		{table: "purchase_orders", check: "RV-PO-01", minRows: 3},
		{table: "po_components", check: "RV-PT-01", minRows: 9},
		{table: "quotes", check: "RV-QT-01", minRows: 3},
		{table: "quote_components", check: "Review lead 1A", minRows: 9},
		{table: "sales_orders", check: "RV-SO-01", minRows: 3},
		{table: "sales_order_components", check: "RV-SO-TRACK-1A", minRows: 9},
	} {
		resp := get(t, client, ts.URL+"/tables/"+tc.table+"?limit=40")
		body := readBody(t, resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s panel status = %d, want 200", tc.table, resp.StatusCode)
		}
		if !strings.Contains(body, tc.check) {
			t.Fatalf("%s panel missing %q: %s", tc.table, tc.check, body)
		}

		apiResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/"+tc.table+"?limit=40", token, nil)
		if apiResp.StatusCode != http.StatusOK {
			t.Fatalf("%s api status = %d, want 200", tc.table, apiResp.StatusCode)
		}

		var payload apiResponse
		decodeJSON(t, apiResp.Body, &payload)
		if len(payload.Rows) < tc.minRows {
			t.Fatalf("%s expected at least %d rows, got %d", tc.table, tc.minRows, len(payload.Rows))
		}
	}

	t.Logf("review dataset seeded at %s", ts.DBPath)
}

func TestRunStopsWhenContextCancelled(t *testing.T) {
	tempDir := t.TempDir()
	server, err := New(context.Background(), Config{
		Addr:   "127.0.0.1:18081",
		DBPath: filepath.Join(tempDir, "stockit.db"),
	})
	if err != nil {
		t.Fatalf("new app server: %v", err)
	}
	defer func() { _ = server.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- server.Run(ctx)
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned error after cancel: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after context cancel")
	}
}

type testServer struct {
	URL    string
	DBPath string

	server *httptest.Server
	app    *Server
}

func (ts *testServer) Close() {
	ts.server.Close()
	_ = ts.app.Close()
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()

	dbPath, cleanup := testDBPath(t)
	t.Cleanup(cleanup)

	server, err := New(context.Background(), Config{
		Addr:   "127.0.0.1:0",
		DBPath: dbPath,
	})
	if err != nil {
		t.Fatalf("new app server: %v", err)
	}

	httpServer := httptest.NewServer(server.Handler())
	return &testServer{
		URL:    httpServer.URL,
		DBPath: dbPath,
		server: httpServer,
		app:    server,
	}
}

func newTLSTestServer(t *testing.T) *testServer {
	t.Helper()

	dbPath, cleanup := testDBPath(t)
	t.Cleanup(cleanup)

	server, err := New(context.Background(), Config{
		Addr:   "127.0.0.1:0",
		DBPath: dbPath,
	})
	if err != nil {
		t.Fatalf("new app server: %v", err)
	}

	httpServer := httptest.NewTLSServer(server.Handler())
	return &testServer{
		URL:    httpServer.URL,
		DBPath: dbPath,
		server: httpServer,
		app:    server,
	}
}

func testDBPath(t *testing.T) (string, func()) {
	t.Helper()

	if keepTestDatabase() {
		if explicitPath := strings.TrimSpace(*testDBPathFlag); explicitPath != "" {
			explicitPath = resolveFromRepoRoot(explicitPath)
			parentDir := filepath.Dir(explicitPath)
			if err := os.MkdirAll(parentDir, 0o755); err != nil {
				t.Fatalf("mkdir review db dir: %v", err)
			}
			cleanupSQLiteFiles(t, explicitPath)
			t.Logf("keeping populated test database at %s", explicitPath)
			return explicitPath, func() {}
		}

		baseDir := filepath.Join(repoRoot(t), "testdata", "review-db")
		if customDir := strings.TrimSpace(*testDBDir); customDir != "" {
			baseDir = resolveFromRepoRoot(customDir)
		}
		if err := os.MkdirAll(baseDir, 0o755); err != nil {
			t.Fatalf("mkdir review db dir: %v", err)
		}

		fileName := sanitizeFileName(t.Name()) + ".db"
		dbPath := filepath.Join(baseDir, fileName)
		cleanupSQLiteFiles(t, dbPath)
		t.Logf("keeping populated test database at %s", dbPath)
		return dbPath, func() {}
	}

	tempDir := t.TempDir()
	return filepath.Join(tempDir, "stockit.db"), func() {}
}

func keepTestDatabase() bool {
	return *testKeepDB
}

func cleanupSQLiteFiles(t *testing.T, dbPath string) {
	t.Helper()

	for _, target := range []string{dbPath, dbPath + "-shm", dbPath + "-wal"} {
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove stale sqlite file %s: %v", target, err)
		}
	}
}

func sanitizeFileName(value string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9._-]+`)
	return re.ReplaceAllString(value, "_")
}

func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve repo root: runtime.Caller failed")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func resolveFromRepoRoot(path string) string {
	if filepath.IsAbs(path) {
		return path
	}

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return path
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	return filepath.Join(root, path)
}

func newHTTPClient(t *testing.T) *http.Client {
	t.Helper()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("new cookie jar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func newServerHTTPClient(t *testing.T, srv *httptest.Server) *http.Client {
	t.Helper()

	if strings.HasPrefix(srv.URL, "https://") {
		client := srv.Client()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("new cookie jar: %v", err)
		}
		client.Jar = jar
		client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}
		return client
	}

	return newHTTPClient(t)
}

func login(t *testing.T, client *http.Client, baseURL, username, password string) {
	t.Helper()

	resp := postForm(t, client, baseURL+"/login", url.Values{
		"login_name": {username},
		"password":   {password},
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", resp.StatusCode, http.StatusSeeOther)
	}
	_ = resp.Body.Close()
}

func createRecord(t *testing.T, client *http.Client, token, baseURL, table string, payload map[string]any) string {
	t.Helper()

	resp := doAPI(t, client, http.MethodPost, baseURL+"/api/tables/"+table, token, payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create %s status = %d, want 201", table, resp.StatusCode)
	}

	var apiPayload apiResponse
	decodeJSON(t, resp.Body, &apiPayload)
	idValue := fmt.Sprint(apiPayload.Row[idColumn(table)])
	if idValue == "" {
		t.Fatalf("create %s missing id: %+v", table, apiPayload.Row)
	}
	return idValue
}

func updateRecord(t *testing.T, client *http.Client, token, baseURL, table, id string, payload map[string]any) {
	t.Helper()

	resp := doAPI(t, client, http.MethodPut, baseURL+"/api/tables/"+table+"/"+id, token, payload)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update %s status = %d, want 200", table, resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func postCSV(t *testing.T, client *http.Client, target, fileName, content string) *http.Response {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fileWriter, err := writer.CreateFormFile("csv_file", fileName)
	if err != nil {
		t.Fatalf("create csv part: %v", err)
	}
	if _, err := io.Copy(fileWriter, strings.NewReader(content)); err != nil {
		t.Fatalf("write csv content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, target, &body)
	if err != nil {
		t.Fatalf("new csv request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post csv %s: %v", target, err)
	}
	return resp
}

func doAPI(t *testing.T, client *http.Client, method, target, token string, payload any) *http.Response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal api payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("new api request %s %s: %v", method, target, err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do api request %s %s: %v", method, target, err)
	}
	return resp
}

func doJSON(t *testing.T, client *http.Client, method, target string, headers map[string]string, payload any) *http.Response {
	t.Helper()

	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal json payload: %v", err)
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequest(method, target, body)
	if err != nil {
		t.Fatalf("new json request %s: %v", target, err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do json request %s: %v", target, err)
	}
	return resp
}

func doMCP(t *testing.T, client *http.Client, target, token, sessionID string, payload any) *http.Response {
	t.Helper()

	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	if sessionID != "" {
		headers["Mcp-Session-Id"] = sessionID
	}
	return doJSON(t, client, http.MethodPost, target, headers, payload)
}

func apiLogin(t *testing.T, client *http.Client, baseURL, loginName, password string) apiLoginResponse {
	t.Helper()

	resp := doJSON(t, client, http.MethodPost, baseURL+"/api/auth/login", nil, apiLoginRequest{
		LoginName: loginName,
		Password:  password,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("api login status = %d, want 200", resp.StatusCode)
	}

	var payload apiLoginResponse
	decodeJSON(t, resp.Body, &payload)
	return payload
}

func extractMCPToolNames(t *testing.T, payload map[string]any) []string {
	t.Helper()

	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing mcp result: %+v", payload)
	}

	rawTools, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("missing tools list: %+v", payload)
	}

	names := make([]string, 0, len(rawTools))
	for _, rawTool := range rawTools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			t.Fatalf("invalid tool payload: %+v", rawTool)
		}
		name, _ := tool["name"].(string)
		names = append(names, name)
	}
	return names
}

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()

	return postFormWithHeaders(t, client, target, values, nil)
}

func postFormWithHeaders(t *testing.T, client *http.Client, target string, values url.Values, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, target, strings.NewReader(values.Encode()))
	if err != nil {
		t.Fatalf("new form request %s: %v", target, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("post form %s: %v", target, err)
	}
	return resp
}

func get(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()

	return getWithHeaders(t, client, target, nil)
}

func getWithHeaders(t *testing.T, client *http.Client, target string, headers map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("new GET request %s: %v", target, err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("get %s: %v", target, err)
	}
	return resp
}

func sessionCookieValue(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()

	parsedURL, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}

	for _, cookie := range client.Jar.Cookies(parsedURL) {
		if cookie.Name == sessionCookieName {
			return cookie.Value
		}
	}
	t.Fatal("session cookie not found")
	return ""
}

func decodeJSON(t *testing.T, body io.ReadCloser, target any) {
	t.Helper()
	defer body.Close()

	if err := json.UnmarshalRead(body, target); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func readBody(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()

	content, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(content)
}

func idColumn(table string) string {
	switch table {
	case "users":
		return "usr_id"
	case "customers":
		return "cus_id"
	case "suppliers":
		return "sup_id"
	case "locations":
		return "loc_id"
	case "items":
		return "itm_id"
	case "boms":
		return "bom_id"
	case "bom_components":
		return "boc_id"
	case "purchase_orders":
		return "por_id"
	case "po_components":
		return "poc_id"
	case "quotes":
		return "qot_id"
	case "quote_components":
		return "qoc_id"
	case "sales_orders":
		return "sor_id"
	case "sales_order_components":
		return "soc_id"
	case "manufacturing_orders":
		return "mfo_id"
	case "mfo_components":
		return "mfc_id"
	case "invoices":
		return "inv_id"
	case "invoice_components":
		return "ivc_id"
	case "adjustments":
		return "adj_id"
	case "adjustment_components":
		return "adc_id"
	case "stock_moves":
		return "stm_id"
	case "bank_accounts":
		return "bnk_id"
	case "designation_codes":
		return "dsg_id"
	case "financial_obligations":
		return "fob_id"
	case "bank_transactions":
		return "btx_id"
	default:
		return "id"
	}
}

func TestCashPlanningRESTFlow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	client := newServerHTTPClient(t, ts.server)
	login := apiLogin(t, client, ts.URL, "admin", "admin")

	account := createRecord(t, client, login.Token, ts.URL, "bank_accounts", map[string]any{
		"bnk_name": "Operating USD", "bnk_currency": "USD",
	})
	obligation := createRecord(t, client, login.Token, ts.URL, "financial_obligations", map[string]any{
		"fob_type": "payable", "fob_source_type": "purchase_order", "fob_label": "prepay",
		"fob_due_date": "2026-08-20", "fob_amount_minor": 25000, "fob_currency": "USD", "fob_status": "planned",
	})
	tx := createRecord(t, client, login.Token, ts.URL, "bank_transactions", map[string]any{
		"bnk_id": account, "btx_date": "2026-08-20", "btx_amount_minor": -25000,
		"btx_designation_code": "GOODS", "fob_id": obligation, "btx_reconciliation_status": "reconciled",
	})
	if tx == "" {
		t.Fatal("bank transaction id is empty")
	}

	bad := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/financial_obligations", login.Token, map[string]any{
		"fob_type": "payable", "fob_source_type": "purchase_order", "fob_label": "invalid",
		"fob_due_date": "2026-08-20", "fob_amount_minor": 0, "fob_currency": "USD", "fob_status": "planned",
	})
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("zero obligation amount status = %d, want 400", bad.StatusCode)
	}
	_ = bad.Body.Close()
}

func TestCashPlanningMCPFlow(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	client := newServerHTTPClient(t, ts.server)
	login := apiLogin(t, client, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, client, ts.URL, login.Token)

	result := mcpCallTool(t, client, ts.URL, login.Token, sessionID, mcpToolCreateRecord, map[string]any{
		"table":  "bank_accounts",
		"values": map[string]any{"bnk_name": "Operating TWD", "bnk_currency": "TWD"},
	})
	if result["isError"] == true {
		t.Fatalf("mcp bank account create failed: %+v", result)
	}
	list := mcpCallTool(t, client, ts.URL, login.Token, sessionID, mcpToolListRecords, map[string]any{
		"table": "bank_accounts", "limit": 10,
	})
	if list["isError"] == true {
		t.Fatalf("mcp bank account list failed: %+v", list)
	}
}

func TestSecurityHeaders(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	resp := get(t, client, ts.URL+"/login")
	_ = resp.Body.Close()

	headers := []struct {
		name  string
		want  string
		exact bool
	}{
		{"X-Content-Type-Options", "nosniff", true},
		{"X-Frame-Options", "SAMEORIGIN", true},
		{"Referrer-Policy", "strict-origin-when-cross-origin", true},
		{"Content-Security-Policy", "default-src 'self'", false},
	}

	for _, h := range headers {
		got := resp.Header.Get(h.name)
		if h.exact {
			if got != h.want {
				t.Errorf("header %s = %q, want %q", h.name, got, h.want)
			}
		} else {
			if !strings.Contains(got, h.want) {
				t.Errorf("header %s = %q, does not contain %q", h.name, got, h.want)
			}
		}
	}

	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("unexpected HSTS header over plain HTTP: %q", got)
	}
}

func TestLoginRateLimiting(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)

	// Attempt login 11 times (limit is 10)
	for i := 1; i <= 11; i++ {
		resp := postForm(t, client, ts.URL+"/login", url.Values{
			"login_name": {"admin"},
			"password":   {"wrong"},
		})
		body := readBody(t, resp.Body)

		if i <= 10 {
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
			}
		} else {
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("attempt %d: status = %d, want 429", i, resp.StatusCode)
			}
			if !strings.Contains(body, "Too many login attempts.") {
				t.Fatalf("unexpected rate limit body: %s", body)
			}
		}
	}
}

func TestForwardedProtoIsIgnoredByDefault(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)

	resp := getWithHeaders(t, client, ts.URL+"/login", map[string]string{
		"X-Forwarded-Proto": "https",
	})
	_ = resp.Body.Close()

	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("unexpected HSTS header when X-Forwarded-Proto is spoofed: %q", got)
	}

	loginResp := postFormWithHeaders(t, client, ts.URL+"/login", url.Values{
		"login_name": {"admin"},
		"password":   {"admin"},
	}, map[string]string{
		"X-Forwarded-Proto": "https",
	})
	defer loginResp.Body.Close()

	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", loginResp.StatusCode, http.StatusSeeOther)
	}

	var sessionCookie *http.Cookie
	for _, cookie := range loginResp.Cookies() {
		if cookie.Name == sessionCookieName {
			sessionCookie = cookie
			break
		}
	}
	if sessionCookie == nil {
		t.Fatal("session cookie missing from login response")
	}
	if sessionCookie.Secure {
		t.Fatal("session cookie should not become Secure from spoofed X-Forwarded-Proto")
	}
}

func TestLoginRateLimitingIgnoresSpoofedForwardedFor(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)

	for i := 1; i <= 11; i++ {
		resp := postFormWithHeaders(t, client, ts.URL+"/login", url.Values{
			"login_name": {"admin"},
			"password":   {"wrong"},
		}, map[string]string{
			"X-Forwarded-For": fmt.Sprintf("198.51.100.%d", i),
		})
		body := readBody(t, resp.Body)

		if i <= 10 {
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("attempt %d: status = %d, want 401", i, resp.StatusCode)
			}
			continue
		}

		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status = %d, want 429", i, resp.StatusCode)
		}
		if !strings.Contains(body, "Too many login attempts.") {
			t.Fatalf("unexpected rate limit body: %s", body)
		}
	}
}

func TestDatabaseErrorSanitization(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")

	// Create a user to cause a unique constraint violation
	_ = postForm(t, client, ts.URL+"/tables/users/save", url.Values{
		"usr_login_name": {"newuser"},
		"usr_password":   {"password"},
		"usr_role":       {"user"},
	})

	// Attempt to create the same user again
	resp := postForm(t, client, ts.URL+"/tables/users/save", url.Values{
		"usr_login_name": {"newuser"},
		"usr_password":   {"password"},
		"usr_role":       {"user"},
	})
	body := readBody(t, resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("duplicate user status = %d, want 200 (form re-rendered with error)", resp.StatusCode)
	}

	// The error message should be in the modal action bar
	errorMsgPattern := regexp.MustCompile(`<span class="stockit-modal-error">(.*?)</span>`)
	matches := errorMsgPattern.FindStringSubmatch(body)
	if len(matches) < 2 {
		t.Fatalf("could not find error message in response: %s", body)
	}
	errorMsg := matches[1]

	// Should NOT contain technical details like "UNIQUE constraint failed" or "users."
	leakyPhrases := []string{"UNIQUE constraint failed", "users."}
	for _, phrase := range leakyPhrases {
		if strings.Contains(errorMsg, phrase) {
			t.Errorf("error message leaks technical detail %q: %s", phrase, errorMsg)
		}
	}

	if !strings.Contains(errorMsg, "A record with this information already exists.") {
		t.Errorf("error message missing expected sanitized text: %s", errorMsg)
	}
}

// initMCPSession performs the JSON-RPC initialize handshake and returns the
// Mcp-Session-Id header that subsequent requests must include.
func initMCPSession(t *testing.T, client *http.Client, baseURL, token string) string {
	t.Helper()
	resp := doMCP(t, client, baseURL+"/mcp", token, "", map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"clientInfo":      map[string]any{"name": "stockit-test", "version": "1.0.0"},
			"capabilities":    map[string]any{},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mcp initialize status = %d, want 200", resp.StatusCode)
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		t.Fatal("missing Mcp-Session-Id header")
	}
	return sessionID
}

// mcpCallTool issues a tools/call JSON-RPC request and returns the parsed result map.
func mcpCallTool(t *testing.T, client *http.Client, baseURL, token, sessionID, name string, arguments map[string]any) map[string]any {
	t.Helper()
	if arguments == nil {
		arguments = map[string]any{}
	}
	resp := doMCP(t, client, baseURL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	})
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp.Body)
		t.Fatalf("tools/call %s status = %d, want 200, body=%s", name, resp.StatusCode, body)
	}
	var payload map[string]any
	decodeJSON(t, resp.Body, &payload)
	if rpcErr, ok := payload["error"]; ok && rpcErr != nil {
		t.Fatalf("tools/call %s rpc error: %+v", name, rpcErr)
	}
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call %s missing result: %+v", name, payload)
	}
	return result
}

func mcpListTools(t *testing.T, client *http.Client, baseURL, token, sessionID string) []string {
	t.Helper()
	resp := doMCP(t, client, baseURL+"/mcp", token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("tools/list status = %d, want 200", resp.StatusCode)
	}
	var payload map[string]any
	decodeJSON(t, resp.Body, &payload)
	return extractMCPToolNames(t, payload)
}

func TestMCPCRUDFlowExercisesEveryTool(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, client, ts.URL, loginResp.Token)

	tools := mcpListTools(t, client, ts.URL, loginResp.Token, sessionID)
	wantTools := []string{
		mcpToolListTables, mcpToolDescribe, mcpToolListRecords,
		mcpToolGetRecord, mcpToolCreateRecord, mcpToolUpdateRecord, mcpToolDeleteRecord,
	}
	for _, name := range wantTools {
		if !slices.Contains(tools, name) {
			t.Fatalf("admin tools/list missing %q: %v", name, tools)
		}
	}

	// describe_table
	describeResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolDescribe, map[string]any{
		"table": "customers",
	})
	describeStruct, ok := describeResult["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("describe missing structuredContent: %+v", describeResult)
	}
	tableMeta, ok := describeStruct["table"].(map[string]any)
	if !ok || tableMeta["name"] != "customers" {
		t.Fatalf("describe unexpected payload: %+v", describeStruct)
	}

	// create_record
	createResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolCreateRecord, map[string]any{
		"table": "customers",
		"values": map[string]any{
			"cus_name_en": "MCP CRUD Customer",
			"cus_status":  "Active",
		},
	})
	createStruct := createResult["structuredContent"].(map[string]any)
	createdID, ok := createStruct["id"].(string)
	if !ok || createdID == "" {
		t.Fatalf("create missing id: %+v", createStruct)
	}

	// get_record
	getResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolGetRecord, map[string]any{
		"table": "customers",
		"id":    createdID,
	})
	getStruct := getResult["structuredContent"].(map[string]any)
	if getStruct["id"] != createdID {
		t.Fatalf("get id mismatch: %+v", getStruct)
	}
	row := getStruct["row"].(map[string]any)
	if row["cus_name_en"] != "MCP CRUD Customer" {
		t.Fatalf("get row unexpected: %+v", row)
	}

	// list_records — verify pagination args are accepted and the new row appears
	listResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolListRecords, map[string]any{
		"table":  "customers",
		"sort":   "cus_name_en",
		"desc":   false,
		"limit":  float64(50),
		"offset": float64(0),
	})
	listStruct := listResult["structuredContent"].(map[string]any)
	rows, ok := listStruct["rows"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("list rows empty: %+v", listStruct)
	}
	foundCreated := false
	for _, raw := range rows {
		entry := raw.(map[string]any)
		if fmt.Sprint(entry["cus_id"]) == createdID {
			foundCreated = true
			break
		}
	}
	if !foundCreated {
		t.Fatalf("list_records did not include created row %s: %+v", createdID, listStruct)
	}

	// update_record — change the name
	updateResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolUpdateRecord, map[string]any{
		"table": "customers",
		"id":    createdID,
		"values": map[string]any{
			"cus_name_en": "Updated MCP Customer",
		},
	})
	updateStruct := updateResult["structuredContent"].(map[string]any)
	updatedRow := updateStruct["row"].(map[string]any)
	if updatedRow["cus_name_en"] != "Updated MCP Customer" {
		t.Fatalf("update did not persist: %+v", updatedRow)
	}

	// delete_record
	deleteResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolDeleteRecord, map[string]any{
		"table": "customers",
		"id":    createdID,
	})
	deleteStruct := deleteResult["structuredContent"].(map[string]any)
	if deleteStruct["deleted"] != true || deleteStruct["id"] != createdID {
		t.Fatalf("delete unexpected payload: %+v", deleteStruct)
	}

	// get_record again — should now report a tool error (404 mapped to error result)
	missingResult := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolGetRecord, map[string]any{
		"table": "customers",
		"id":    createdID,
	})
	if missingResult["isError"] != true {
		t.Fatalf("get after delete should be a tool error: %+v", missingResult)
	}
}

func TestMCPListRecordsValidatesLimit(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, client, ts.URL, loginResp.Token)

	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "limit too large", args: map[string]any{"table": "customers", "limit": float64(9999)}},
		{name: "negative offset", args: map[string]any{"table": "customers", "offset": float64(-1)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := mcpCallTool(t, client, ts.URL, loginResp.Token, sessionID, mcpToolListRecords, tc.args)
			if result["isError"] != true {
				t.Fatalf("expected tool error, got %+v", result)
			}
		})
	}
}

func TestMCPGuestCannotInvokeWriteTools(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "guest", "guest")
	sessionID := initMCPSession(t, client, ts.URL, loginResp.Token)

	tools := mcpListTools(t, client, ts.URL, loginResp.Token, sessionID)
	for _, forbidden := range []string{mcpToolCreateRecord, mcpToolUpdateRecord, mcpToolDeleteRecord} {
		if slices.Contains(tools, forbidden) {
			t.Fatalf("guest tools/list should not expose %q: %v", forbidden, tools)
		}
	}

	// Even when invoked directly by name, the server must reject the call.
	resp := doMCP(t, client, ts.URL+"/mcp", loginResp.Token, sessionID, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params": map[string]any{
			"name": mcpToolCreateRecord,
			"arguments": map[string]any{
				"table": "customers",
				"values": map[string]any{
					"cus_name_en": "Should Not Persist",
				},
			},
		},
	})
	var payload map[string]any
	decodeJSON(t, resp.Body, &payload)
	if payload["error"] != nil {
		return
	}
	result, ok := payload["result"].(map[string]any)
	if !ok {
		t.Fatalf("guest create_record missing rejection: %+v", payload)
	}
	if result["isError"] != true {
		t.Fatalf("guest create_record should be a tool error: %+v", result)
	}
}

func TestMCPRejectsCrossOriginCookieRequest(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// Log in via the HTML form so the session lives in the cookie jar.
	htmlClient := newHTTPClient(t)
	login(t, htmlClient, ts.URL, "admin", "admin")

	// First request must succeed (same-origin) so we know auth/state are healthy.
	sessionID := initMCPSession(t, htmlClient, ts.URL, "")
	if sessionID == "" {
		t.Fatal("expected initialize to succeed for same-origin cookie auth")
	}

	// Now the same client issues a cross-origin POST. The CrossOriginProtection
	// wrapper added in routes() must reject it with 403.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}`))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Mcp-Session-Id", sessionID)

	resp, err := htmlClient.Do(req)
	if err != nil {
		t.Fatalf("cross-origin mcp request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin mcp status = %d, want 403", resp.StatusCode)
	}
}

func TestAPIRejectsBlankRequiredField(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	token := loginResp.Token

	// Create with explicit empty required field → 400.
	createBlank := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/customers", token, map[string]any{
		"cus_name_en": "   ",
	})
	if createBlank.StatusCode != http.StatusBadRequest {
		t.Fatalf("create blank required status = %d, want 400", createBlank.StatusCode)
	}
	var blankErr apiErrorResponse
	decodeJSON(t, createBlank.Body, &blankErr)
	if !strings.Contains(blankErr.Error, "required") {
		t.Fatalf("expected required error, got %+v", blankErr)
	}

	// Create a real row.
	createOK := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/customers", token, map[string]any{
		"cus_name_en": "Required Field Customer",
	})
	if createOK.StatusCode != http.StatusCreated {
		t.Fatalf("create ok status = %d, want 201", createOK.StatusCode)
	}
	var created apiTableRowResponse
	decodeJSON(t, createOK.Body, &created)
	if created.ID == "" {
		t.Fatalf("created response missing id: %+v", created)
	}

	// Update that explicitly blanks the required field → 400.
	updateBlank := doAPI(t, client, http.MethodPut, ts.URL+"/api/tables/customers/"+created.ID, token, map[string]any{
		"cus_name_en": "",
	})
	if updateBlank.StatusCode != http.StatusBadRequest {
		t.Fatalf("update blank required status = %d, want 400", updateBlank.StatusCode)
	}

	// Partial update that omits the required field → 200 (still partial-update friendly).
	updateOK := doAPI(t, client, http.MethodPut, ts.URL+"/api/tables/customers/"+created.ID, token, map[string]any{
		"cus_status": "Inactive",
	})
	if updateOK.StatusCode != http.StatusOK {
		body := readBody(t, updateOK.Body)
		t.Fatalf("partial update status = %d, want 200, body=%s", updateOK.StatusCode, body)
	}
}

func TestAPIDuplicateUserReturns409(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	token := loginResp.Token

	first := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/users", token, map[string]any{
		"usr_login_name": "dup-test",
		"usr_password":   "password",
		"usr_role":       "user",
	})
	if first.StatusCode != http.StatusCreated {
		body := readBody(t, first.Body)
		t.Fatalf("first user create status = %d, want 201, body=%s", first.StatusCode, body)
	}
	_ = first.Body.Close()

	dup := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/users", token, map[string]any{
		"usr_login_name": "dup-test",
		"usr_password":   "password",
		"usr_role":       "user",
	})
	defer dup.Body.Close()
	if dup.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate user status = %d, want 409", dup.StatusCode)
	}
}

func TestClassifyStoreErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: 0},
		{name: "unique", err: fmt.Errorf("UNIQUE constraint failed: users.usr_login_name"), want: 409},
		{name: "foreign", err: fmt.Errorf("FOREIGN KEY constraint failed"), want: 409},
		{name: "check", err: fmt.Errorf("CHECK constraint failed: status"), want: 400},
		{name: "not null", err: fmt.Errorf("NOT NULL constraint failed: cus_name_en"), want: 400},
		{name: "other", err: fmt.Errorf("disk i/o error"), want: 500},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyStoreError(tc.err); got != tc.want {
				t.Fatalf("classifyStoreError(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestAPIAuthLogoutInvalidatesToken(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	token := loginResp.Token

	// Sanity-check that the token works before logout.
	meBefore := getWithHeaders(t, client, ts.URL+"/api/me", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if meBefore.StatusCode != http.StatusOK {
		t.Fatalf("me before logout status = %d, want 200", meBefore.StatusCode)
	}
	_ = meBefore.Body.Close()

	// Log out.
	logoutResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/auth/logout", token, nil)
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", logoutResp.StatusCode)
	}
	clearedCookieFound := false
	for _, cookie := range logoutResp.Cookies() {
		if cookie.Name == sessionCookieName && cookie.MaxAge < 0 {
			clearedCookieFound = true
		}
	}
	_ = logoutResp.Body.Close()
	if !clearedCookieFound {
		t.Fatal("logout response did not clear the session cookie")
	}

	// Token must no longer work.
	meAfter := getWithHeaders(t, client, ts.URL+"/api/me", map[string]string{
		"Authorization": "Bearer " + token,
	})
	defer meAfter.Body.Close()
	if meAfter.StatusCode != http.StatusUnauthorized {
		t.Fatalf("me after logout status = %d, want 401", meAfter.StatusCode)
	}
}

func TestAPIPaginationAndSort(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, client, ts.URL, "admin", "admin")
	token := loginResp.Token

	// Seed enough rows that pagination can be observed.
	const seed = 35
	for i := 0; i < seed; i++ {
		resp := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/customers", token, map[string]any{
			"cus_name_en": fmt.Sprintf("Pager %03d", i),
			"cus_status":  "Active",
		})
		if resp.StatusCode != http.StatusCreated {
			body := readBody(t, resp.Body)
			t.Fatalf("seed row %d status = %d, want 201, body=%s", i, resp.StatusCode, body)
		}
		_ = resp.Body.Close()
	}

	// Request the first page sorted descending by name with a small limit.
	page1 := getWithHeaders(t, client, ts.URL+"/api/tables/customers?limit=10&offset=0&sort=cus_name_en&desc=true", map[string]string{
		"Authorization": "Bearer " + token,
	})
	if page1.StatusCode != http.StatusOK {
		t.Fatalf("page1 status = %d, want 200", page1.StatusCode)
	}
	var page1Body apiTableListResponse
	decodeJSON(t, page1.Body, &page1Body)
	if len(page1Body.Rows) != 10 {
		t.Fatalf("page1 row count = %d, want 10", len(page1Body.Rows))
	}
	if !page1Body.HasMore {
		t.Fatal("page1 should have more")
	}
	if fmt.Sprint(page1Body.Rows[0]["cus_name_en"]) != "Pager 034" {
		t.Fatalf("page1 first row = %v, want \"Pager 034\"", page1Body.Rows[0]["cus_name_en"])
	}

	// Reject obviously bad limits.
	for _, query := range []string{"limit=0", "limit=999", "offset=-1"} {
		resp := getWithHeaders(t, client, ts.URL+"/api/tables/customers?"+query, map[string]string{
			"Authorization": "Bearer " + token,
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q status = %d, want 400", query, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestAPISessionLimitReached(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	for i := 0; i < maxSessionsPerUser; i++ {
		client := newServerHTTPClient(t, ts.server)
		_ = apiLogin(t, client, ts.URL, "admin", "admin")
	}

	// One more login should hit the session limit and return 403 with the error envelope.
	overflowClient := newServerHTTPClient(t, ts.server)
	resp := doJSON(t, overflowClient, http.MethodPost, ts.URL+"/api/auth/login", nil, apiLoginRequest{
		LoginName: "admin",
		Password:  "admin",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("session-limit login status = %d, want 403", resp.StatusCode)
	}
	var body apiErrorResponse
	decodeJSON(t, resp.Body, &body)
	if !strings.Contains(body.Error, "session limit") {
		t.Fatalf("unexpected error envelope: %+v", body)
	}
}

func TestStockMovesSrcDstValidationOnAPIAndMCP(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	item := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":          "STM-PARITY-ITM",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	loc := createRecord(t, client, token, ts.URL, "locations", map[string]any{
		"loc_name":   "STM-PARITY-LOC",
		"loc_zone":   "storage",
		"loc_status": "Active",
	})

	// REST: same src and dst must be rejected with 400.
	badPayload := map[string]any{
		"stm_doc_number": "STM-PARITY-REST",
		"stm_date":       "2026-04-10",
		"itm_id":         item,
		"stm_src_loc_id": loc,
		"stm_dst_loc_id": loc,
		"stm_qty":        1,
	}
	apiResp := doAPI(t, client, http.MethodPost, ts.URL+"/api/tables/stock_moves", token, badPayload)
	if apiResp.StatusCode != http.StatusBadRequest {
		body := readBody(t, apiResp.Body)
		t.Fatalf("rest stock_moves same-loc status = %d, want 400, body=%s", apiResp.StatusCode, body)
	}
	var apiErr apiErrorResponse
	decodeJSON(t, apiResp.Body, &apiErr)
	if !strings.Contains(apiErr.Error, "Source and destination locations must be different") {
		t.Fatalf("rest error missing validator message: %+v", apiErr)
	}

	// REST update path must enforce the same check when both fields are present.
	goodID := createRecord(t, client, token, ts.URL, "stock_moves", map[string]any{
		"stm_doc_number": "STM-PARITY-UPDATE",
		"stm_date":       "2026-04-11",
		"itm_id":         item,
		"stm_dst_loc_id": loc,
		"stm_qty":        2,
	})
	updateResp := doAPI(t, client, http.MethodPut, ts.URL+"/api/tables/stock_moves/"+goodID, token, map[string]any{
		"stm_src_loc_id": loc,
		"stm_dst_loc_id": loc,
	})
	if updateResp.StatusCode != http.StatusBadRequest {
		body := readBody(t, updateResp.Body)
		t.Fatalf("rest stock_moves update same-loc status = %d, want 400, body=%s", updateResp.StatusCode, body)
	}
	_ = updateResp.Body.Close()

	// MCP: same-location create must return a tool error.
	mcpClient := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, mcpClient, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, mcpClient, ts.URL, loginResp.Token)
	createResult := mcpCallTool(t, mcpClient, ts.URL, loginResp.Token, sessionID, mcpToolCreateRecord, map[string]any{
		"table": "stock_moves",
		"values": map[string]any{
			"stm_doc_number": "STM-PARITY-MCP",
			"stm_date":       "2026-04-12",
			"itm_id":         item,
			"stm_src_loc_id": loc,
			"stm_dst_loc_id": loc,
			"stm_qty":        3,
		},
	})
	if createResult["isError"] != true {
		t.Fatalf("mcp create_record same-loc should be a tool error: %+v", createResult)
	}
}

func TestAPIAndMCPParentFilterForSubtables(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	final := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":    "PF-FG-1",
		"itm_type":   "final",
		"itm_status": "Active",
	})
	part := createRecord(t, client, token, ts.URL, "items", map[string]any{
		"itm_sku":    "PF-PT-1",
		"itm_type":   "part",
		"itm_status": "Active",
	})
	bomA := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "PF-BOM-A",
		"itm_id":         final,
		"bom_status":     "Active",
	})
	bomB := createRecord(t, client, token, ts.URL, "boms", map[string]any{
		"bom_doc_number": "PF-BOM-B",
		"itm_id":         final,
		"bom_status":     "Active",
	})
	compA := createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
		"bom_id":  bomA,
		"itm_id":  part,
		"boc_qty": 1,
	})
	_ = createRecord(t, client, token, ts.URL, "bom_components", map[string]any{
		"bom_id":  bomB,
		"itm_id":  part,
		"boc_qty": 2,
	})

	// REST: parent filter narrows to bomA only.
	filteredResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/bom_components?parent_id="+bomA, token, nil)
	if filteredResp.StatusCode != http.StatusOK {
		body := readBody(t, filteredResp.Body)
		t.Fatalf("rest filtered list status = %d, want 200, body=%s", filteredResp.StatusCode, body)
	}
	var filteredBody apiTableListResponse
	decodeJSON(t, filteredResp.Body, &filteredBody)
	if len(filteredBody.Rows) != 1 {
		t.Fatalf("rest filtered rows = %d, want 1, payload=%+v", len(filteredBody.Rows), filteredBody)
	}
	if fmt.Sprint(filteredBody.Rows[0]["boc_id"]) != compA {
		t.Fatalf("rest filtered row id = %v, want %s", filteredBody.Rows[0]["boc_id"], compA)
	}
	if fmt.Sprint(filteredBody.Rows[0]["bom_id"]) != bomA {
		t.Fatalf("rest filtered row bom_id = %v, want %s", filteredBody.Rows[0]["bom_id"], bomA)
	}

	// REST: parent filter on a non-subtable must fail.
	badResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/customers?parent_id=1", token, nil)
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("parent_id on non-subtable status = %d, want 400", badResp.StatusCode)
	}
	_ = badResp.Body.Close()

	// REST: parent_field that does not match declared parent is rejected.
	badFieldResp := doAPI(t, client, http.MethodGet, ts.URL+"/api/tables/bom_components?parent_id="+bomA+"&parent_field=itm_id", token, nil)
	if badFieldResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched parent_field status = %d, want 400", badFieldResp.StatusCode)
	}
	_ = badFieldResp.Body.Close()

	// MCP: same filter via list_records.
	mcpClient := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, mcpClient, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, mcpClient, ts.URL, loginResp.Token)
	mcpResult := mcpCallTool(t, mcpClient, ts.URL, loginResp.Token, sessionID, mcpToolListRecords, map[string]any{
		"table":     "bom_components",
		"parent_id": bomB,
	})
	structured, ok := mcpResult["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("mcp filtered result missing structuredContent: %+v", mcpResult)
	}
	rows, ok := structured["rows"].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("mcp filtered rows = %+v, want 1", structured)
	}
	entry := rows[0].(map[string]any)
	if fmt.Sprint(entry["bom_id"]) != bomB {
		t.Fatalf("mcp filtered row bom_id = %v, want %s", entry["bom_id"], bomB)
	}
}

func TestCSVImportViaAPIAndMCP(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)
	login(t, client, ts.URL, "admin", "admin")
	token := sessionCookieValue(t, client, ts.URL)

	// REST multipart import.
	csvBody := "cus_name_en,cus_phone,cus_status\n" +
		"REST Import One,111,Active\n" +
		"REST Import Two,222,Hold\n"
	req := newCSVUploadRequest(t, http.MethodPost, ts.URL+"/api/tables/customers/import", "customers.csv", csvBody)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("rest import: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body := readBody(t, resp.Body)
		t.Fatalf("rest import status = %d, want 200, body=%s", resp.StatusCode, body)
	}
	var importBody apiTableImportResponse
	decodeJSON(t, resp.Body, &importBody)
	if importBody.Table != "customers" || importBody.Imported != 2 {
		t.Fatalf("rest import payload = %+v, want customers/2", importBody)
	}

	// MCP import tool.
	mcpClient := newServerHTTPClient(t, ts.server)
	loginResp := apiLogin(t, mcpClient, ts.URL, "admin", "admin")
	sessionID := initMCPSession(t, mcpClient, ts.URL, loginResp.Token)
	mcpResult := mcpCallTool(t, mcpClient, ts.URL, loginResp.Token, sessionID, mcpToolImportCSV, map[string]any{
		"table": "customers",
		"csv": "cus_name_en,cus_phone,cus_status\n" +
			"MCP Import One,333,Active\n" +
			"MCP Import Two,444,Under Review\n",
	})
	mcpStruct, ok := mcpResult["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("mcp import missing structuredContent: %+v", mcpResult)
	}
	if fmt.Sprint(mcpStruct["imported"]) != "2" || mcpStruct["table"] != "customers" {
		t.Fatalf("mcp import payload = %+v", mcpStruct)
	}

	// Business-rule validation applies to CSV import too.
	loc1 := createRecord(t, client, token, ts.URL, "locations", map[string]any{
		"loc_name":   "CSV-LOC-1",
		"loc_zone":   "storage",
		"loc_status": "Active",
	})
	badCSV := "stm_doc_number,stm_date,stm_src_loc_id,stm_dst_loc_id,stm_qty\n" +
		"CSV-BAD-MOVE,2026-04-12," + loc1 + "," + loc1 + ",1\n"
	badResult := mcpCallTool(t, mcpClient, ts.URL, loginResp.Token, sessionID, mcpToolImportCSV, map[string]any{
		"table": "stock_moves",
		"csv":   badCSV,
	})
	if badResult["isError"] != true {
		t.Fatalf("csv import with same-loc stock move should fail: %+v", badResult)
	}

	// Guest must be forbidden from the REST import endpoint.
	guestClient := newHTTPClient(t)
	login(t, guestClient, ts.URL, "guest", "guest")
	guestToken := sessionCookieValue(t, guestClient, ts.URL)
	guestReq := newCSVUploadRequest(t, http.MethodPost, ts.URL+"/api/tables/customers/import", "guest.csv", "cus_name_en\nGuest Co\n")
	guestReq.Header.Set("Authorization", "Bearer "+guestToken)
	guestResp, err := guestClient.Do(guestReq)
	if err != nil {
		t.Fatalf("guest import: %v", err)
	}
	if guestResp.StatusCode != http.StatusForbidden {
		body := readBody(t, guestResp.Body)
		t.Fatalf("guest rest import status = %d, want 403, body=%s", guestResp.StatusCode, body)
	}
	_ = guestResp.Body.Close()
}

func newCSVUploadRequest(t *testing.T, method, target, fileName, content string) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	fileWriter, err := writer.CreateFormFile("csv_file", fileName)
	if err != nil {
		t.Fatalf("create csv part: %v", err)
	}
	if _, err := io.Copy(fileWriter, strings.NewReader(content)); err != nil {
		t.Fatalf("write csv content: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req, err := http.NewRequest(method, target, &body)
	if err != nil {
		t.Fatalf("new csv request: %v", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestAPIRejectsMalformedJSONBodies(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	client := newHTTPClient(t)

	cases := []struct {
		name string
		body string
	}{
		{name: "duplicate member", body: `{"login_name":"admin","login_name":"admin","password":"admin"}`},
		{name: "trailing data", body: `{"login_name":"admin","password":"admin"}{}`},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/auth/login", strings.NewReader(testCase.body))
			if err != nil {
				t.Fatalf("new login request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")

			resp, err := client.Do(req)
			if err != nil {
				t.Fatalf("do login request: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("login status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
			}
		})
	}
}
