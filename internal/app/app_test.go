package app

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestBOMSubtableFlowUsesParentContext(t *testing.T) {
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
	if strings.Contains(childPanelBody, "Beta component") {
		t.Fatalf("child panel should exclude components from other BOMs: %s", childPanelBody)
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
		createRecord(t, client, token, ts.URL, "customers", payload)
	}

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
		createRecord(t, client, token, ts.URL, "suppliers", payload)
	}

	for _, payload := range []map[string]any{
		{"loc_name": "Main Warehouse", "loc_zone": "storage", "loc_status": "Active"},
		{"loc_name": "Assembly Floor", "loc_zone": "assembly", "loc_status": "Active"},
		{"loc_name": "Returns Cage", "loc_zone": "returns", "loc_status": "Hold"},
	} {
		createRecord(t, client, token, ts.URL, "locations", payload)
	}

	itemIDs := make([]string, 0, 3)
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
			"itm_sku":          "RV-PT-01",
			"itm_model":        "Review Part",
			"itm_description":  "Component for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        10.25,
			"itm_status":       "Active",
		},
	} {
		itemIDs = append(itemIDs, createRecord(t, client, token, ts.URL, "items", payload))
	}

	bomIDs := make([]string, 0, 3)
	for _, payload := range []map[string]any{
		{"bom_doc_number": "RV-BOM-01", "itm_id": itemIDs[0], "bom_note": "Review BOM A", "bom_status": "Under Review"},
		{"bom_doc_number": "RV-BOM-02", "itm_id": itemIDs[1], "bom_note": "Review BOM B", "bom_status": "Active"},
		{"bom_doc_number": "RV-BOM-03", "itm_id": itemIDs[0], "bom_note": "Review BOM C", "bom_status": "Draft"},
	} {
		bomIDs = append(bomIDs, createRecord(t, client, token, ts.URL, "boms", payload))
	}

	for _, payload := range []map[string]any{
		{"bom_id": bomIDs[0], "itm_id": itemIDs[2], "boc_qty": 5, "boc_note": "Review component line A"},
		{"bom_id": bomIDs[1], "itm_id": itemIDs[2], "boc_qty": 8, "boc_note": "Review component line B"},
		{"bom_id": bomIDs[2], "itm_id": itemIDs[2], "boc_qty": 2, "boc_note": "Review component line C"},
	} {
		createRecord(t, client, token, ts.URL, "bom_components", payload)
	}

	for _, tc := range []struct {
		table   string
		check   string
		minRows int
	}{
		{table: "customers", check: "Review Customer A", minRows: 3},
		{table: "suppliers", check: "Review Supplier A", minRows: 3},
		{table: "locations", check: "Main Warehouse", minRows: 3},
		{table: "items", check: "RV-FG-01", minRows: 3},
		{table: "boms", check: "RV-BOM-01", minRows: 3},
		{table: "bom_components", check: "Review component line A", minRows: 3},
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

func postForm(t *testing.T, client *http.Client, target string, values url.Values) *http.Response {
	t.Helper()

	resp, err := client.PostForm(target, values)
	if err != nil {
		t.Fatalf("post form %s: %v", target, err)
	}
	return resp
}

func get(t *testing.T, client *http.Client, target string) *http.Response {
	t.Helper()

	resp, err := client.Get(target)
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

	if err := json.NewDecoder(body).Decode(target); err != nil {
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
	default:
		return "id"
	}
}
