package main

import (
	"bytes"
	"encoding/json/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"testing"
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
			writeJSON(w, http.StatusOK, stockitLoginResponse{Token: "stockit-token", User: "user", Role: "user"})
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
		case "/api/tables/financial_obligations":
			if !requireBearer(t, w, r) {
				return
			}
			writeJSON(w, http.StatusOK, tableRowsResponse{Rows: []map[string]any{
				{"fob_id": int64(41), "fob_type": "payable", "fob_status": "due", "fob_currency": "USD", "fob_due_date": "2026-08-10", "fob_amount_minor": int64(200), "fob_label": "Overdue rent", "fob_counterparty": "Landlord", "fob_source_type": "other"},
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
	if loginBody["user"] != "user" || loginBody["role"] != "user" || loginBody["token"] != "" {
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
	if len(bodyResponse.Forecast) != 3 || !bodyResponse.Forecast[0].Overdue || bodyResponse.Forecast[0].ProjectedBalanceMinor != 7300 || bodyResponse.Forecast[1].ProjectedBalanceMinor != 6300 || bodyResponse.Forecast[2].ProjectedBalanceMinor != 6800 || len(bodyResponse.Forecast[0].Details) != 1 || bodyResponse.Forecast[0].Details[0].Label != "Overdue rent" {
		t.Fatalf("unexpected forecast: %+v", bodyResponse.Forecast)
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
