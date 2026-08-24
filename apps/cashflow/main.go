package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json/v2"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var files embed.FS

const sessionCookie = "stockit_cashflow_session"

const dateLayout = "2006-01-02"

type app struct {
	stockitURL string
	client     *http.Client
	mu         sync.Mutex
	// ponytail: local sessions remain until logout or failed StockIt request; add TTL pruning for long-lived deployment.
	sessions map[string]session
}

type session struct {
	token string
	user  string
	role  string
}

type loginRequest struct {
	LoginName string `json:"login_name"`
	Password  string `json:"password"`
}

type stockitLoginResponse struct {
	Token string `json:"token"`
	User  string `json:"user"`
	Role  string `json:"role"`
}

type tableRowsResponse struct {
	Rows    []map[string]any `json:"rows"`
	HasMore bool             `json:"has_more"`
}

type accountSummary struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Currency     string `json:"currency"`
	BalanceMinor int64  `json:"balance_minor"`
}

type obligationDetail struct {
	ID           int64  `json:"id"`
	Label        string `json:"label"`
	Counterparty string `json:"counterparty"`
	SourceType   string `json:"source_type"`
	DueDate      string `json:"due_date"`
	Type         string `json:"type"`
	Status       string `json:"status"`
	AmountMinor  int64  `json:"amount_minor"`
}

type forecastEntry struct {
	Currency              string             `json:"currency"`
	DueDate               string             `json:"due_date"`
	Overdue               bool               `json:"overdue"`
	PayableMinor          int64              `json:"payable_minor"`
	ReceivableMinor       int64              `json:"receivable_minor"`
	NetMinor              int64              `json:"net_minor"`
	ProjectedBalanceMinor int64              `json:"projected_balance_minor"`
	Details               []obligationDetail `json:"details"`
}

type cashflowResponse struct {
	User            string           `json:"user"`
	Role            string           `json:"role"`
	AsOfDate        string           `json:"as_of_date"`
	ForecastThrough string           `json:"forecast_through"`
	Accounts        []accountSummary `json:"accounts"`
	Forecast        []forecastEntry  `json:"forecast"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "cashflow listen address")
	stockitURL := flag.String("stockit-url", "http://127.0.0.1:8080", "StockIt base URL")
	flag.Parse()

	if _, err := url.ParseRequestURI(*stockitURL); err != nil {
		log.Fatalf("invalid -stockit-url: %v", err)
	}
	app := newApp(*stockitURL, &http.Client{Timeout: 15 * time.Second})
	log.Printf("cashflow app listening on http://%s; StockIt=%s", *addr, app.stockitURL)
	log.Fatal(http.ListenAndServe(*addr, app.handler()))
}

func newApp(stockitURL string, client *http.Client) *app {
	return &app{
		stockitURL: strings.TrimRight(stockitURL, "/"),
		client:     client,
		sessions:   make(map[string]session),
	}
}

func (a *app) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", a.handleIndex)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /api/cashflow", a.handleCashflow)
	return mux
}

func (a *app) handleIndex(w http.ResponseWriter, _ *http.Request) {
	html, err := files.ReadFile("index.html")
	if err != nil {
		http.Error(w, "load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(html)
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	var credentials loginRequest
	if err := json.UnmarshalRead(r.Body, &credentials); err != nil || strings.TrimSpace(credentials.LoginName) == "" || strings.TrimSpace(credentials.Password) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "login_name and password are required"})
		return
	}

	body, err := json.Marshal(credentials)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encode login"})
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.stockitURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create StockIt login request"})
		return
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "StockIt is unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		writeJSON(w, response.StatusCode, map[string]string{"error": "StockIt login failed"})
		return
	}

	var login stockitLoginResponse
	if err := json.UnmarshalRead(response.Body, &login); err != nil || login.Token == "" {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "invalid StockIt login response"})
		return
	}
	appToken, err := randomToken()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create app session"})
		return
	}
	a.mu.Lock()
	a.sessions[appToken] = session{token: login.Token, user: login.User, role: login.Role}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: appToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]string{"user": login.User, "role": login.Role})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	appToken, current, ok := a.sessionFromRequest(r)
	if ok {
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.stockitURL+"/api/auth/logout", nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+current.token)
			response, err := a.client.Do(request)
			if err == nil {
				_ = response.Body.Close()
			}
		}
		a.mu.Lock()
		delete(a.sessions, appToken)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) handleCashflow(w http.ResponseWriter, r *http.Request) {
	appToken, current, ok := a.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	asOf, through, err := reportDates(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	accounts, status, err := a.listRows(r.Context(), "bank_accounts", current.token)
	if err == nil {
		transactions, transactionStatus, transactionErr := a.listRows(r.Context(), "bank_transactions", current.token)
		if transactionErr == nil {
			obligations, obligationStatus, obligationErr := a.listRows(r.Context(), "financial_obligations", current.token)
			if obligationErr == nil {
				writeJSON(w, http.StatusOK, buildCashflow(current, accounts, transactions, obligations, asOf, through))
				return
			}
			status, err = obligationStatus, obligationErr
		} else {
			status, err = transactionStatus, transactionErr
		}
	}
	if status == http.StatusUnauthorized {
		a.mu.Lock()
		delete(a.sessions, appToken)
		a.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "StockIt session expired; log in again"})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]string{"error": "StockIt data request failed"})
}

func reportDates(r *http.Request) (time.Time, time.Time, error) {
	asOf := time.Now().In(time.Local)
	if raw := r.URL.Query().Get("as_of"); raw != "" {
		parsed, err := time.Parse(dateLayout, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("as_of must use YYYY-MM-DD")
		}
		asOf = parsed
	}
	through := asOf.AddDate(0, 0, 90)
	if raw := r.URL.Query().Get("through"); raw != "" {
		parsed, err := time.Parse(dateLayout, raw)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("through must use YYYY-MM-DD")
		}
		through = parsed
	}
	if through.Before(asOf) {
		return time.Time{}, time.Time{}, fmt.Errorf("through must be on or after as_of")
	}
	return asOf, through, nil
}

func (a *app) sessionFromRequest(r *http.Request) (string, session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return "", session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	current, ok := a.sessions[cookie.Value]
	return cookie.Value, current, ok
}

func (a *app) listRows(ctx context.Context, table, token string) ([]map[string]any, int, error) {
	var rows []map[string]any
	for offset := 0; ; {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/tables/%s?limit=200&offset=%d", a.stockitURL, table, offset), nil)
		if err != nil {
			return nil, 0, err
		}
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := a.client.Do(request)
		if err != nil {
			return nil, 0, err
		}
		if response.StatusCode != http.StatusOK {
			_ = response.Body.Close()
			return nil, response.StatusCode, fmt.Errorf("StockIt %s returned %d", table, response.StatusCode)
		}
		var page tableRowsResponse
		err = json.UnmarshalRead(response.Body, &page)
		_ = response.Body.Close()
		if err != nil {
			return nil, 0, err
		}
		rows = append(rows, page.Rows...)
		if !page.HasMore {
			return rows, http.StatusOK, nil
		}
		if len(page.Rows) == 0 {
			return nil, 0, fmt.Errorf("StockIt %s returned empty page with has_more", table)
		}
		offset += len(page.Rows)
	}
}

func buildCashflow(current session, accountRows, transactionRows, obligationRows []map[string]any, asOf, through time.Time) cashflowResponse {
	type account struct {
		id       int64
		name     string
		currency string
		balance  int64
	}
	accounts := make(map[int64]*account, len(accountRows))
	for _, row := range accountRows {
		id, ok := intValue(row["bnk_id"])
		if !ok {
			continue
		}
		accounts[id] = &account{id: id, name: stringValue(row["bnk_name"]), currency: stringValue(row["bnk_currency"])}
	}
	for _, row := range transactionRows {
		accountID, accountOK := intValue(row["bnk_id"])
		amount, amountOK := intValue(row["btx_amount_minor"])
		date, dateErr := time.Parse(dateLayout, stringValue(row["btx_date"]))
		if accountOK && amountOK && dateErr == nil && !date.After(asOf) && accounts[accountID] != nil {
			accounts[accountID].balance += amount
		}
	}

	result := cashflowResponse{User: current.user, Role: current.role, AsOfDate: asOf.Format(dateLayout), ForecastThrough: through.Format(dateLayout)}
	balances := make(map[string]int64)
	for _, account := range accounts {
		result.Accounts = append(result.Accounts, accountSummary{ID: account.id, Name: account.name, Currency: account.currency, BalanceMinor: account.balance})
		balances[account.currency] += account.balance
	}
	sort.Slice(result.Accounts, func(i, j int) bool { return result.Accounts[i].Name < result.Accounts[j].Name })

	type event struct {
		date                string
		overdue             bool
		payable, receivable int64
		details             []obligationDetail
	}
	events := make(map[string]map[string]event)
	for _, row := range obligationRows {
		status := stringValue(row["fob_status"])
		if status == "paid" || status == "cancelled" {
			continue
		}
		amount, amountOK := intValue(row["fob_amount_minor"])
		currency, dueDate := stringValue(row["fob_currency"]), stringValue(row["fob_due_date"])
		due, dueErr := time.Parse(dateLayout, dueDate)
		if !amountOK || amount <= 0 || currency == "" || dueErr != nil {
			continue
		}
		overdue := due.Before(asOf)
		forecastDate := due
		if overdue {
			forecastDate = asOf
		}
		if forecastDate.After(through) {
			continue
		}
		byDate := events[currency]
		if byDate == nil {
			byDate = make(map[string]event)
			events[currency] = byDate
		}
		key := forecastDate.Format(dateLayout) + "|"
		if overdue {
			key += "0"
		} else {
			key += "1"
		}
		entry := byDate[key]
		entry.date, entry.overdue = forecastDate.Format(dateLayout), overdue
		detail := obligationDetail{
			ID:           valueOrZero(row["fob_id"]),
			Label:        stringValue(row["fob_label"]),
			Counterparty: stringValue(row["fob_counterparty"]),
			SourceType:   stringValue(row["fob_source_type"]),
			DueDate:      dueDate,
			Type:         stringValue(row["fob_type"]),
			Status:       status,
			AmountMinor:  amount,
		}
		switch stringValue(row["fob_type"]) {
		case "payable":
			entry.payable += amount
		case "receivable":
			entry.receivable += amount
		default:
			continue
		}
		entry.details = append(entry.details, detail)
		byDate[key] = entry
	}

	currencies := make(map[string]bool, len(balances)+len(events))
	for currency := range balances {
		currencies[currency] = true
	}
	for currency := range events {
		currencies[currency] = true
	}
	for currency := range currencies {
		dates := make([]string, 0, len(events[currency]))
		for key := range events[currency] {
			dates = append(dates, key)
		}
		sort.Strings(dates)
		projected := balances[currency]
		for _, key := range dates {
			event := events[currency][key]
			net := event.receivable - event.payable
			projected += net
			result.Forecast = append(result.Forecast, forecastEntry{Currency: currency, DueDate: event.date, Overdue: event.overdue, PayableMinor: event.payable, ReceivableMinor: event.receivable, NetMinor: net, ProjectedBalanceMinor: projected, Details: event.details})
		}
	}
	sort.Slice(result.Forecast, func(i, j int) bool {
		if result.Forecast[i].Currency == result.Forecast[j].Currency {
			return result.Forecast[i].DueDate < result.Forecast[j].DueDate
		}
		return result.Forecast[i].Currency < result.Forecast[j].Currency
	})
	return result
}

func intValue(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case float64:
		return int64(value), value == float64(int64(value))
	case string:
		parsed, err := strconv.ParseInt(value, 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func valueOrZero(value any) int64 {
	result, _ := intValue(value)
	return result
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func randomToken() (string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.MarshalWrite(w, value)
}
