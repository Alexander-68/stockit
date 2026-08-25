package main

import (
	"bytes"
	"embed"
	"encoding/json/v2"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"
)

//go:embed index.html
var files embed.FS

const sessionCookie = "stockit_purchasing_session"

// tableMethods whitelists the StockIt tables this app may touch and with which
// HTTP methods. StockIt still enforces role permissions behind the proxy.
var tableMethods = map[string]string{
	"suppliers":             "GET POST PUT DELETE",
	"items":                 "GET",
	"quotes":                "GET",
	"quote_components":      "GET",
	"purchase_orders":       "GET POST PUT DELETE",
	"po_status_history":     "GET",
	"po_components":         "GET POST PUT DELETE",
	"financial_obligations": "GET POST PUT DELETE",
}

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
	role  string
	// approvalLimitMinor is the largest purchase order this user may approve, in
	// integer minor units; it decides whether the UI offers Approve or Submit.
	approvalLimitMinor int64
}

// workflowActions whitelists the StockIt approval endpoints this app may call.
// Each entry is "<collection>/<action>"; StockIt still enforces role and state.
var workflowActions = map[string]bool{
	"purchase_orders/submit":  true,
	"purchase_orders/status":  true,
	"purchase_orders/approve": true,
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

type stockitMeResponse struct {
	ApprovalLimitMinor int64 `json:"approval_limit_minor"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8091", "purchasing listen address")
	stockitURL := flag.String("stockit-url", "http://127.0.0.1:8080", "StockIt base URL")
	secureCookies := flag.Bool("secure-cookies", false, "always set Secure on the session cookie (use behind an HTTPS proxy)")
	flag.Parse()

	if _, err := url.ParseRequestURI(*stockitURL); err != nil {
		log.Fatalf("invalid -stockit-url: %v", err)
	}
	app := newApp(*stockitURL, &http.Client{Timeout: 15 * time.Second})
	app.secureCookies = *secureCookies
	log.Printf("purchasing app listening on http://%s; StockIt=%s", *addr, app.stockitURL)
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
	mux.HandleFunc("GET /{$}", a.handleIndex)
	mux.HandleFunc("GET /assets/", a.handleAssets)
	mux.HandleFunc("POST /login", a.handleLogin)
	mux.HandleFunc("POST /logout", a.handleLogout)
	mux.HandleFunc("GET /api/me", a.handleMe)
	mux.HandleFunc("/api/tables/", a.handleProxy)
	mux.HandleFunc("POST /api/purchase_orders/{id}/{action}", a.handleWorkflowProxy)
	return mux
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
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
	limit := a.approvalLimit(r, login.Token)
	a.mu.Lock()
	a.sessions[appToken] = session{token: login.Token, user: login.User, role: login.Role, approvalLimitMinor: limit}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: appToken, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: a.secureCookies || r.TLS != nil})
	writeJSON(w, http.StatusOK, map[string]any{"user": login.User, "role": login.Role, "approval_limit_minor": limit})
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

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	_, current, ok := a.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user": current.user, "role": current.role, "approval_limit_minor": current.approvalLimitMinor,
	})
}

// approvalLimit reads the signing authority StockIt holds for the user who just
// logged in. A failure is not fatal: a zero limit only means the UI offers
// "Submit for approval" instead of "Approve", and StockIt checks it again.
func (a *app) approvalLimit(r *http.Request, token string) int64 {
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.stockitURL+"/api/me", nil)
	if err != nil {
		return 0
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := a.client.Do(request)
	if err != nil {
		return 0
	}
	defer response.Body.Close()
	var me stockitMeResponse
	if err := json.UnmarshalRead(response.Body, &me); err != nil {
		return 0
	}
	return me.ApprovalLimitMinor
}

// handleProxy forwards whitelisted /api/tables requests to StockIt with the
// session's bearer token. StockIt validates payloads and role permissions.
func (a *app) handleProxy(w http.ResponseWriter, r *http.Request) {
	appToken, current, ok := a.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/tables/")
	table, id, hasID := strings.Cut(rest, "/")
	methods, allowed := tableMethods[table]
	if !allowed || !strings.Contains(methods, r.Method) || strings.Contains(id, "/") || id == "" && hasID {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not allowed"})
		return
	}
	target := a.stockitURL + "/api/tables/" + table
	if hasID {
		target += "/" + url.PathEscape(id)
	}
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	a.forward(w, r, appToken, current, target)
}

// handleWorkflowProxy forwards the whitelisted StockIt approval actions. Path
// shape is fixed by the route, so only the collection and action need checking.
func (a *app) handleWorkflowProxy(w http.ResponseWriter, r *http.Request) {
	appToken, current, ok := a.sessionFromRequest(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "login required"})
		return
	}
	collection, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/api/"), "/")
	id, action := r.PathValue("id"), r.PathValue("action")
	if !workflowActions[collection+"/"+action] || id == "" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not allowed"})
		return
	}
	a.forward(w, r, appToken, current, a.stockitURL+"/api/"+collection+"/"+url.PathEscape(id)+"/"+action)
}

// forward relays one request to StockIt with the session's bearer token and
// copies the response back, dropping the local session when StockIt rejects it.
func (a *app) forward(w http.ResponseWriter, r *http.Request, appToken string, current session, target string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "create StockIt request"})
		return
	}
	request.Header.Set("Authorization", "Bearer "+current.token)
	if contentType := r.Header.Get("Content-Type"); contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := a.client.Do(request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "StockIt is unavailable"})
		return
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusUnauthorized {
		a.mu.Lock()
		delete(a.sessions, appToken)
		a.mu.Unlock()
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "StockIt session expired; log in again"})
		return
	}
	if contentType := response.Header.Get("Content-Type"); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
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
