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

func TestPurchaseOrderSubtableFlowUsesParentContext(t *testing.T) {
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

func TestQuoteSubtableFlowUsesParentContext(t *testing.T) {
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

func TestSalesOrderSubtableFlowUsesParentContext(t *testing.T) {
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

	for _, payload := range []map[string]any{
		{"loc_name": "Main Warehouse", "loc_zone": "storage", "loc_status": "Active"},
		{"loc_name": "Assembly Floor", "loc_zone": "assembly", "loc_status": "Active"},
		{"loc_name": "Returns Cage", "loc_zone": "returns", "loc_status": "Hold"},
	} {
		createRecord(t, client, token, ts.URL, "locations", payload)
	}

	itemIDs := make([]string, 0, 20)
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
		{
			"itm_sku":          "RV-AS-04",
			"itm_model":        "Review Assembly Delta",
			"itm_description":  "Assembly fixture delta",
			"itm_type":         "assembly",
			"itm_measure_unit": "set",
			"itm_value":        49.9,
			"itm_status":       "Hold",
		},
		{
			"itm_sku":          "RV-FG-04",
			"itm_model":        "Review Final D",
			"itm_description":  "Finished good for review D",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        165.0,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-FG-05",
			"itm_model":        "Review Final E",
			"itm_description":  "Finished good for review E",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        172.5,
			"itm_status":       "Under Review",
		},
		{
			"itm_sku":          "RV-FG-06",
			"itm_model":        "Review Final F",
			"itm_description":  "Finished good for review F",
			"itm_type":         "final",
			"itm_measure_unit": "pcs",
			"itm_value":        181.2,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-10",
			"itm_model":        "Review Part Kappa",
			"itm_description":  "Switch housing for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        5.95,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-11",
			"itm_model":        "Review Part Lambda",
			"itm_description":  "Ribbon cable for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        3.95,
			"itm_status":       "Active",
		},
		{
			"itm_sku":          "RV-PT-12",
			"itm_model":        "Review Part Mu",
			"itm_description":  "Connector block for review",
			"itm_type":         "part",
			"itm_measure_unit": "pcs",
			"itm_value":        6.15,
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

	for _, tc := range []struct {
		table   string
		check   string
		minRows int
	}{
		{table: "users", check: "admin", minRows: 3},
		{table: "customers", check: "Review Customer A", minRows: 3},
		{table: "suppliers", check: "Review Supplier A", minRows: 3},
		{table: "locations", check: "Main Warehouse", minRows: 3},
		{table: "items", check: "RV-FG-01", minRows: 20},
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
	default:
		return "id"
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

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("duplicate user status = %d, want 400", resp.StatusCode)
	}

	// The error message should be in a specific div
	errorMsgPattern := regexp.MustCompile(`<div class="stockit-inline-message stockit-inline-message-error">(.*?)</div>`)
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
