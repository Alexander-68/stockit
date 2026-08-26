package main

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCashflowUsesServerSideStockItToken(t *testing.T) {
	stockit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			var credentials loginRequest
			if err := json.UnmarshalRead(r.Body, &credentials); err != nil || credentials.LoginName != "user" || credentials.Password != "password" {
				t.Error("cashflow backend did not forward login credentials")
				http.Error(w, "bad login", http.StatusBadRequest)
				return
			}
			writeJSON(w, http.StatusOK, stockitLoginResponse{Token: "stockit-token", User: "user"})
		case "/api/tables/bank_accounts":
			if !requireBearer(t, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{{"bnk_id": int64(1), "bnk_name": "Operating", "bnk_currency": "USD"}}})
		case "/api/tables/bank_transactions":
			if !requireBearer(t, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{{"bnk_id": int64(1), "btx_date": "2026-08-01", "btx_amount_minor": int64(10000)}, {"bnk_id": int64(1), "btx_date": "2026-08-12", "btx_amount_minor": int64(-2500)}, {"bnk_id": int64(1), "btx_date": "2026-09-02", "btx_amount_minor": int64(3000)}}})
		case "/api/tables/designation_codes":
			if !requireBearer(t, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{
				{"dsg_id": int64(9), "dsg_code": "RENT", "dsg_name": "Rent", "dsg_status": "Active"},
				{"dsg_id": int64(10), "dsg_code": "OLD_FEE", "dsg_name": "Retired fee", "dsg_status": "Inactive"},
			}})
		case "/api/tables/financial_obligations":
			if !requireBearer(t, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{
				{"fob_id": int64(41), "fob_type": "payable", "fob_status": "due", "fob_currency": "USD", "fob_due_date": "2026-08-10", "fob_amount_minor": int64(200), "fob_label": "Overdue rent", "fob_counterparty": "Landlord", "fob_source_type": "other", "fob_designation_code": "RENT"},
				{"fob_type": "payable", "fob_status": "due", "fob_currency": "USD", "fob_due_date": "2026-08-24", "fob_amount_minor": int64(300)},
				{"fob_type": "payable", "fob_status": "due", "fob_currency": "USD", "fob_due_date": "2026-09-01", "fob_amount_minor": int64(1000)},
				{"fob_type": "receivable", "fob_status": "planned", "fob_currency": "USD", "fob_due_date": "2026-09-02", "fob_amount_minor": int64(500)},
				{"fob_type": "payable", "fob_status": "paid", "fob_currency": "USD", "fob_due_date": "2026-09-03", "fob_amount_minor": int64(999)},
				{"fob_type": "payable", "fob_status": "planned", "fob_currency": "USD", "fob_due_date": "2026-10-01", "fob_amount_minor": int64(999)},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer stockit.Close()

	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	body, err := json.Marshal(loginRequest{LoginName: "user", Password: "password"})
	if err != nil {
		t.Fatal(err)
	}
	loginResponse, err := client.Post(appServer.URL+"/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer loginResponse.Body.Close()
	if loginResponse.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", loginResponse.StatusCode)
	}
	var loginBody map[string]string
	if err := json.UnmarshalRead(loginResponse.Body, &loginBody); err != nil {
		t.Fatal(err)
	}
	if loginBody["user"] != "user" || loginBody["token"] != "" {
		t.Fatalf("unexpected login response: %+v", loginBody)
	}

	response, err := client.Get(appServer.URL + "/api/cashflow?as_of=2026-08-24&through=2026-09-02")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("cashflow status = %d, want 200", response.StatusCode)
	}
	var bodyResponse cashflowResponse
	if err := json.UnmarshalRead(response.Body, &bodyResponse); err != nil {
		t.Fatal(err)
	}
	if len(bodyResponse.Accounts) != 1 || bodyResponse.Accounts[0].BalanceMinor != 7500 {
		t.Fatalf("unexpected accounts: %+v", bodyResponse.Accounts)
	}
	if bodyResponse.AsOfDate != "2026-08-24" || bodyResponse.ForecastThrough != "2026-09-02" {
		t.Fatalf("unexpected report dates: %+v", bodyResponse)
	}
	want := []forecastEntry{
		{DueDate: "2026-08-24", Overdue: true, ProjectedBalanceMinor: 7300},
		{DueDate: "2026-08-24", Overdue: false, ProjectedBalanceMinor: 7000},
		{DueDate: "2026-09-01", Overdue: false, ProjectedBalanceMinor: 6000},
		{DueDate: "2026-09-02", Overdue: false, ProjectedBalanceMinor: 6500},
	}
	if len(bodyResponse.Forecast) != len(want) {
		t.Fatalf("unexpected forecast: %+v", bodyResponse.Forecast)
	}
	for i, entry := range want {
		got := bodyResponse.Forecast[i]
		if got.DueDate != entry.DueDate || got.Overdue != entry.Overdue || got.ProjectedBalanceMinor != entry.ProjectedBalanceMinor {
			t.Fatalf("forecast[%d] = %+v, want %+v", i, got, entry)
		}
	}
	if len(bodyResponse.Forecast[0].Details) != 1 || bodyResponse.Forecast[0].Details[0].Label != "Overdue rent" {
		t.Fatalf("unexpected overdue details: %+v", bodyResponse.Forecast[0].Details)
	}
	if len(bodyResponse.Opening) != 1 || bodyResponse.Opening[0].Currency != "USD" || bodyResponse.Opening[0].OpeningMinor != 7500 || !bodyResponse.Opening[0].HasAccount {
		t.Fatalf("unexpected opening balances: %+v", bodyResponse.Opening)
	}
}

func TestReportDatesDefaultToCalendarDay(t *testing.T) {
	today, err := time.Parse(dateLayout, time.Now().Format(dateLayout))
	if err != nil {
		t.Fatal(err)
	}
	asOf, through, err := reportDates(httptest.NewRequest(http.MethodGet, "/api/cashflow", nil))
	if err != nil {
		t.Fatal(err)
	}
	if !asOf.Equal(today) {
		t.Fatalf("as_of = %v, want %v", asOf, today)
	}
	// An obligation due today is not yet overdue.
	if today.Before(asOf) {
		t.Fatal("obligation due on the as-of date is treated as overdue")
	}
	if !through.Equal(today.AddDate(0, 0, 90)) {
		t.Fatalf("through = %v, want as_of+90d", through)
	}
	// Today is a valid forecast-through value when as_of defaults to today.
	if _, _, err := reportDates(httptest.NewRequest(http.MethodGet, "/api/cashflow?through="+today.Format(dateLayout), nil)); err != nil {
		t.Fatalf("through=today rejected: %v", err)
	}
}

func TestListRowsPagesWithStableSort(t *testing.T) {
	var sorts []string
	stockit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sorts = append(sorts, r.URL.Query().Get("sort"))
		if r.URL.Query().Get("offset") == "0" {
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{{"fob_id": int64(1)}, {"fob_id": int64(2)}}, HasMore: true})
			return
		}
		writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{{"fob_id": int64(3)}}})
	}))
	defer stockit.Close()

	rows, status, err := newApp(stockit.URL, stockit.Client()).listRows(t.Context(), "financial_obligations", "fob_id", "token")
	if err != nil || status != http.StatusOK {
		t.Fatalf("listRows: status=%d err=%v", status, err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	for _, sort := range sorts {
		if sort != "fob_id" {
			t.Fatalf("sort = %q, want fob_id on every page", sort)
		}
	}
}

func requireBearer(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer stockit-token" {
		t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		http.Error(w, "bad authorization", http.StatusUnauthorized)
		return false
	}
	return true
}

// TestAssetsProxyIsPublic keeps the shared StockIt stylesheet reachable before login so the
// app's sign-in page renders with StockIt's own look.
func TestAssetsProxyIsPublic(t *testing.T) {
	stockit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/assets/") {
			t.Errorf("unexpected StockIt request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(".stockit-login-card{}"))
	}))
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

func TestRecurringDates(t *testing.T) {
	valid := recurringRequest{Label: "Office rent", Type: "payable", Currency: "USD", AmountMinor: 250000, FirstDueDate: "2026-01-31", Period: "monthly", Count: 3}
	dates, err := recurringDates(valid)
	if err != nil {
		t.Fatalf("valid request rejected: %v", err)
	}
	// A month-end start must stay at month end instead of rolling into March.
	if want := []string{"2026-01-31", "2026-02-28", "2026-03-31"}; !slices.Equal(dates, want) {
		t.Fatalf("monthly dates = %v, want %v", dates, want)
	}

	weekly := valid
	weekly.Period, weekly.FirstDueDate = "weekly", "2026-01-01"
	dates, err = recurringDates(weekly)
	if err != nil {
		t.Fatalf("weekly request rejected: %v", err)
	}
	if want := []string{"2026-01-01", "2026-01-08", "2026-01-15"}; !slices.Equal(dates, want) {
		t.Fatalf("weekly dates = %v, want %v", dates, want)
	}

	for name, mutate := range map[string]func(*recurringRequest){
		"blank label":   func(r *recurringRequest) { r.Label = "  " },
		"bad type":      func(r *recurringRequest) { r.Type = "donation" },
		"no currency":   func(r *recurringRequest) { r.Currency = "" },
		"zero amount":   func(r *recurringRequest) { r.AmountMinor = 0 },
		"count too big": func(r *recurringRequest) { r.Count = maxRecurringCount + 1 },
		"bad period":    func(r *recurringRequest) { r.Period = "fortnightly" },
		"bad date":      func(r *recurringRequest) { r.FirstDueDate = "31/01/2026" },
	} {
		invalid := valid
		mutate(&invalid)
		if _, err := recurringDates(invalid); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestRecurringCreatesOneRowPerInstallment(t *testing.T) {
	var created []map[string]any
	stockit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/login":
			writeJSON(w, http.StatusOK, map[string]string{"token": "stockit-token", "user": "alex"})
		case "/api/tables/financial_obligations":
			if got := r.Header.Get("Authorization"); got != "Bearer stockit-token" {
				t.Errorf("Authorization = %q", got)
			}
			var row map[string]any
			if err := json.UnmarshalRead(r.Body, &row); err != nil {
				t.Errorf("decode row: %v", err)
			}
			created = append(created, row)
			writeJSON(w, http.StatusCreated, map[string]any{"row": row})
		default:
			t.Errorf("unexpected StockIt request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer stockit.Close()
	appServer := httptest.NewServer(newApp(stockit.URL, stockit.Client()).handler())
	defer appServer.Close()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}

	response, err := client.Post(appServer.URL+"/login", "application/json", strings.NewReader(`{"login_name":"alex","password":"secret"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	body := `{"label":"Office rent","counterparty":"Landlord","type":"payable","source_type":"other","amount_minor":250000,"currency":"USD","designation_code":"RENT","first_due_date":"2026-03-01","period":"monthly","count":3}`
	response, err = client.Post(appServer.URL+"/api/recurring", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("recurring returned %d, want 200", response.StatusCode)
	}
	if len(created) != 3 {
		t.Fatalf("created %d obligations, want 3", len(created))
	}
	if got := created[2]["fob_due_date"]; got != "2026-05-01" {
		t.Errorf("third due date = %v, want 2026-05-01", got)
	}
	if got := created[0]["fob_status"]; got != "planned" {
		t.Errorf("status = %v, want planned", got)
	}
	if got := created[0]["fob_designation_code"]; got != "RENT" {
		t.Errorf("designation = %v, want RENT", got)
	}
}

func TestCashflowLabelsDesignationsAndListsActiveCodes(t *testing.T) {
	names := map[string]string{"RENT": "Rent"}
	if got := designationLabel("RENT", names); got != "RENT - Rent" {
		t.Errorf("known code = %q, want \"RENT - Rent\"", got)
	}
	// A code the table no longer carries still has to render, or the obligation
	// looks like it has no designation at all.
	if got := designationLabel("GONE", names); got != "GONE" {
		t.Errorf("unknown code = %q, want \"GONE\"", got)
	}
	if got := designationLabel("", names); got != "" {
		t.Errorf("blank code = %q, want empty", got)
	}

	designations := []map[string]any{
		{"dsg_code": "RENT", "dsg_name": "Rent", "dsg_status": "Active"},
		{"dsg_code": "OLD_FEE", "dsg_name": "Retired fee", "dsg_status": "Inactive"},
		{"dsg_code": "LEGACY", "dsg_name": "No status"},
		{"dsg_name": "No code"},
	}
	result := buildCashflow(session{user: "alex"}, nil, nil, nil, designations, time.Now(), time.Now())
	if want := []designationOption{{Code: "LEGACY", Name: "No status"}, {Code: "RENT", Name: "Rent"}}; !slices.Equal(result.Designations, want) {
		t.Fatalf("designations = %+v, want %+v", result.Designations, want)
	}
}

func TestRecurringRequiresLogin(t *testing.T) {
	appServer := httptest.NewServer(newApp("http://127.0.0.1:1", http.DefaultClient).handler())
	defer appServer.Close()
	response, err := http.Post(appServer.URL+"/api/recurring", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous recurring returned %d, want 401", response.StatusCode)
	}
}
