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
	"io"
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
	// secureCookies forces the Secure attribute when TLS terminates in front of this app.
	secureCookies bool
	mu            sync.Mutex
	// ponytail: local sessions remain until logout or failed StockIt request; add TTL pruning for long-lived deployment.
	sessions map[string]session
}

type session struct {
	token string
	user  string
}

type loginRequest struct {
	LoginName string `json:"login_name"`
	Password  string `json:"password"`
}

type stockitLoginResponse struct {
	Token string `json:"token"`
	User  string `json:"user"`
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
	Designation  string `json:"designation"`
}

// designationOption feeds the recurring-payment dropdown: the code is what the
// obligation stores, the name is what a human recognises.
type designationOption struct {
	Code string `json:"code"`
	Name string `json:"name"`
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

type currencyOpening struct {
	Currency     string `json:"currency"`
	OpeningMinor int64  `json:"opening_minor"`
	HasAccount   bool   `json:"has_account"`
}

// recurringRequest is one recurring commitment (rent, payroll, a loan
// installment) the user wants written out as individual obligations.
type recurringRequest struct {
	Label        string `json:"label"`
	Counterparty string `json:"counterparty"`
	Type         string `json:"type"`
	SourceType   string `json:"source_type"`
	AmountMinor  int64  `json:"amount_minor"`
	Currency     string `json:"currency"`
	FirstDueDate string `json:"first_due_date"`
	Designation  string `json:"designation_code"`
	Period       string `json:"period"`
	Count        int    `json:"count"`
	Note         string `json:"note"`
}

type cashflowResponse struct {
	User            string            `json:"user"`
	AsOfDate        string            `json:"as_of_date"`
	ForecastThrough string            `json:"forecast_through"`
	Accounts        []accountSummary  `json:"accounts"`
	Opening         []currencyOpening `json:"opening"`
	Forecast        []forecastEntry   `json:"forecast"`
	// Designations lists the active codes, so the form offers only usable ones.
	Designations []designationOption `json:"designations"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "cashflow listen address")
	stockitURL := flag.String("stockit-url", "http://127.0.0.1:8080", "StockIt base URL")
	secureCookies := flag.Bool("secure-cookies", false, "always set Secure on the session cookie (use behind an HTTPS proxy)")
	flag.Parse()

	if _, err := url.ParseRequestURI(*stockitURL); err != nil {
		log.Fatalf("invalid -stockit-url: %v", err)
	}
	app := newApp(*stockitURL, &http.Client{Timeout: 15 * time.Second})
	app.secureCookies = *secureCookies
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
	mux.HandleFunc("GET /assets/", a.handleAssets)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /api/cashflow", a.handleCashflow)
	mux.HandleFunc("POST /api/recurring", a.handleRecurring)
	return mux
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	html, err := files.ReadFile("index.html")
	if err != nil {
		http.Error(w, "load page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(html)
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
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
	a.sessions[appToken] = session{token: login.Token, user: login.User}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: appToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secureCookies || r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]string{"user": login.User})
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
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secureCookies || r.TLS != nil, MaxAge: -1})
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

	tables := [][2]string{{"bank_accounts", "bnk_id"}, {"bank_transactions", "btx_id"}, {"financial_obligations", "fob_id"}, {"designation_codes", "dsg_id"}}
	loaded := make([][]map[string]any, len(tables))
	for index, table := range tables {
		rows, status, err := a.listRows(r.Context(), table[0], table[1], current.token)
		if err != nil {
			if status == http.StatusUnauthorized {
				a.mu.Lock()
				delete(a.sessions, appToken)
				a.mu.Unlock()
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "StockIt session expired; log in again"})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "StockIt data request failed"})
			return
		}
		loaded[index] = rows
	}
	writeJSON(w, http.StatusOK, buildCashflow(current, loaded[0], loaded[1], loaded[2], loaded[3], asOf, through))
}

// monthsPerPeriod keeps the recurrence rules to whole-month steps, which is what
// rent, payroll and installment plans actually use.
var monthsPerPeriod = map[string]int{"weekly": 0, "monthly": 1, "quarterly": 3, "yearly": 12}

const maxRecurringCount = 60

func (a *app) handleRecurring(w http.ResponseWriter, r *http.Request) {
	appToken, current, ok := a.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var request recurringRequest
	if err := json.UnmarshalRead(r.Body, &request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request body"})
		return
	}
	dates, err := recurringDates(request)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// ponytail: rows are created one by one with no transaction, so a mid-run
	// failure leaves the earlier ones in place; the response says how many
	// landed. Add a StockIt bulk endpoint if partial writes become a problem.
	created := 0
	for _, due := range dates {
		row := map[string]any{
			"fob_type":             request.Type,
			"fob_source_type":      request.SourceType,
			"fob_label":            strings.TrimSpace(request.Label),
			"fob_due_date":         due,
			"fob_amount_minor":     request.AmountMinor,
			"fob_currency":         strings.TrimSpace(request.Currency),
			"fob_status":           "planned",
			"fob_counterparty":     strings.TrimSpace(request.Counterparty),
			"fob_designation_code": nilIfEmpty(strings.TrimSpace(request.Designation)),
			"fob_note":             strings.TrimSpace(request.Note),
		}
		status, err := a.createRow(r.Context(), "financial_obligations", current.token, row)
		if err == nil {
			created++
			continue
		}
		if status == http.StatusUnauthorized {
			a.mu.Lock()
			delete(a.sessions, appToken)
			a.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "StockIt session expired; log in again"})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"created": created, "error": fmt.Sprintf("StockIt rejected the obligation due %s (%d); %d of %d created", due, status, created, len(dates))})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"created": created, "first_due_date": dates[0], "last_due_date": dates[len(dates)-1]})
}

// recurringDates validates the request and returns one due date per installment.
func recurringDates(request recurringRequest) ([]string, error) {
	if strings.TrimSpace(request.Label) == "" {
		return nil, fmt.Errorf("label is required")
	}
	if request.Type != "payable" && request.Type != "receivable" {
		return nil, fmt.Errorf("type must be payable or receivable")
	}
	if strings.TrimSpace(request.Currency) == "" {
		return nil, fmt.Errorf("currency is required")
	}
	if request.AmountMinor <= 0 {
		return nil, fmt.Errorf("amount_minor must be positive")
	}
	if request.Count < 1 || request.Count > maxRecurringCount {
		return nil, fmt.Errorf("count must be between 1 and %d", maxRecurringCount)
	}
	months, ok := monthsPerPeriod[request.Period]
	if !ok {
		return nil, fmt.Errorf("period must be weekly, monthly, quarterly or yearly")
	}
	first, err := time.Parse(dateLayout, request.FirstDueDate)
	if err != nil {
		return nil, fmt.Errorf("first_due_date must use YYYY-MM-DD")
	}
	dates := make([]string, 0, request.Count)
	for index := range request.Count {
		if months == 0 {
			dates = append(dates, first.AddDate(0, 0, 7*index).Format(dateLayout))
			continue
		}
		dates = append(dates, addMonths(first, months*index).Format(dateLayout))
	}
	return dates, nil
}

// addMonths steps whole months and clamps to the last day of the target month,
// so a payment due on the 31st stays month-end instead of rolling into the next
// month the way time.AddDate normalises it.
func addMonths(date time.Time, months int) time.Time {
	target := time.Date(date.Year(), date.Month(), 1, 0, 0, 0, 0, date.Location()).AddDate(0, months, 0)
	lastDay := target.AddDate(0, 1, -1).Day()
	return time.Date(target.Year(), target.Month(), min(date.Day(), lastDay), 0, 0, 0, 0, date.Location())
}

// nilIfEmpty keeps an unset optional column NULL instead of an empty string.
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// createRow posts one row to a StockIt table with the session's bearer token.
func (a *app) createRow(ctx context.Context, table, token string, row map[string]any) (int, error) {
	body, err := json.Marshal(row)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.stockitURL+"/api/tables/"+table, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	response, err := a.client.Do(request)
	if err != nil {
		return http.StatusBadGateway, err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return response.StatusCode, fmt.Errorf("StockIt %s create returned %d", table, response.StatusCode)
	}
	return response.StatusCode, nil
}

// designationLabel renders a stored code with its name, falling back to the bare
// code when the code table no longer has it.
func designationLabel(code string, names map[string]string) string {
	if code == "" {
		return ""
	}
	if name := names[code]; name != "" {
		return code + " - " + name
	}
	return code
}

func reportDates(r *http.Request) (time.Time, time.Time, error) {
	// Report dates are calendar dates: keep them at UTC midnight so they compare
	// with time.Parse(dateLayout, ...) values from StockIt rows.
	now := time.Now()
	asOf := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
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

// listRows pages through a table sorted by its primary key so offset paging cannot
// duplicate or skip rows the way StockIt's non-unique default sort can.
func (a *app) listRows(ctx context.Context, table, primaryKey, token string) ([]map[string]any, int, error) {
	var rows []map[string]any
	for offset := 0; ; {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/api/tables/%s?sort=%s&limit=200&offset=%d", a.stockitURL, table, primaryKey, offset), nil)
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

func buildCashflow(current session, accountRows, transactionRows, obligationRows, designationRows []map[string]any, asOf, through time.Time) cashflowResponse {
	designationNames := make(map[string]string, len(designationRows))
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

	result := cashflowResponse{User: current.user, AsOfDate: asOf.Format(dateLayout), ForecastThrough: through.Format(dateLayout)}
	for _, row := range designationRows {
		code, name := stringValue(row["dsg_code"]), stringValue(row["dsg_name"])
		if code == "" {
			continue
		}
		designationNames[code] = name
		if status := stringValue(row["dsg_status"]); status == "" || status == "Active" {
			result.Designations = append(result.Designations, designationOption{Code: code, Name: name})
		}
	}
	sort.Slice(result.Designations, func(i, j int) bool { return result.Designations[i].Code < result.Designations[j].Code })
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
			Designation:  designationLabel(stringValue(row["fob_designation_code"]), designationNames),
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
		_, hasAccount := balances[currency]
		result.Opening = append(result.Opening, currencyOpening{Currency: currency, OpeningMinor: balances[currency], HasAccount: hasAccount})
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
	sort.Slice(result.Opening, func(i, j int) bool { return result.Opening[i].Currency < result.Opening[j].Currency })
	sort.Slice(result.Forecast, func(i, j int) bool {
		if result.Forecast[i].Currency != result.Forecast[j].Currency {
			return result.Forecast[i].Currency < result.Forecast[j].Currency
		}
		if result.Forecast[i].DueDate != result.Forecast[j].DueDate {
			return result.Forecast[i].DueDate < result.Forecast[j].DueDate
		}
		// Overdue bucket carries the earlier projected balance on the as-of date.
		return result.Forecast[i].Overdue && !result.Forecast[j].Overdue
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

// handleAssets serves StockIt's public stylesheet through this origin so the
// app's login page and chrome match StockIt without duplicating its CSS.
func (a *app) handleAssets(w http.ResponseWriter, r *http.Request) {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.stockitURL+r.URL.Path, nil)
	if err != nil {
		http.Error(w, "asset request", http.StatusInternalServerError)
		return
	}
	response, err := a.client.Do(request)
	if err != nil {
		http.Error(w, "StockIt is unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}
