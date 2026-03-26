package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"stockit/internal/auth"
	"stockit/internal/store"
	"stockit/internal/web"
)

const sessionCookieName = "stockit_session"

type contextKey string

const principalKey contextKey = "principal"

type Config struct {
	Addr   string
	DBPath string
}

type Principal struct {
	UserID    int64
	LoginName string
	Role      string
	Token     string
}

type Server struct {
	cfg        Config
	store      *store.Store
	sessions   *auth.Manager
	templates  *web.Templates
	cop        *http.CrossOriginProtection
	handler    http.Handler
	limitersMu sync.Mutex
	limiters   map[string]rateLimiter
}

type loginPageData struct {
	Error string
}

type dashboardPageData struct {
	User         Principal
	Tables       []store.TableDef
	DefaultTable string
}

type tablePanelData struct {
	User       Principal
	Table      store.TableDef
	NavTable   string
	Headers    []tableHeaderView
	Rows       []tableRowView
	Parent     *parentContextView
	ChildTable string
	ChildField string
	CanWrite   bool
	Sort       string
	Desc       bool
	Limit      int
	HasMore    bool
	RowCount   int
	Message    string
	CanImport  bool
}

type tableHeaderView struct {
	Column string
	Label  string
	Active bool
	Desc   bool
}

type tableRowView struct {
	ID            string
	Cells         []string
	DeleteConfirm string
}

type parentContextView struct {
	TableName  string
	TableLabel string
	RowID      string
	Field      string
	Label      string
	Title      string
	Summary    []parentFieldView
	CanWrite   bool
}

type parentFieldView struct {
	Label string
	Value string
}

type parentContext struct {
	Table    store.TableDef
	Row      map[string]any
	RowID    string
	Field    string
	Label    string
	ParsedID any
}

type formData struct {
	Table                store.TableDef
	User                 Principal
	Parent               *parentContextView
	Fields               []formFieldView
	RowID                string
	Error                string
	CanDelete            bool
	SubmitPath           string
	DeletePath           string
	DeleteConfirmMessage string
}

type formFieldView struct {
	Column      string
	Label       string
	Kind        string
	Value       string
	Required    bool
	Options     []store.Option
	Accept      string
	HasValue    bool
	Help        string
	Visible     bool
	Rows        int
	Autofocus   bool
	Placeholder string
}

type apiResponse struct {
	Table   string           `json:"table,omitempty"`
	Role    string           `json:"role,omitempty"`
	User    string           `json:"user,omitempty"`
	Rows    []map[string]any `json:"rows,omitempty"`
	Row     map[string]any   `json:"row,omitempty"`
	HasMore bool             `json:"has_more,omitempty"`
	Error   string           `json:"error,omitempty"`
}

const (
	loginAttemptWindow  = time.Minute
	loginAttemptLimit   = 10
	loginLimiterMaxIdle = 10 * time.Minute
)

func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:8080"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = filepath.Join("data", "stockit.db")
	}

	dataStore, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return nil, err
	}

	templates, err := web.NewTemplates()
	if err != nil {
		_ = dataStore.Close()
		return nil, err
	}

	srv := &Server{
		cfg:       cfg,
		store:     dataStore,
		sessions:  auth.NewManager(5, 15*time.Minute),
		templates: templates,
		cop:       http.NewCrossOriginProtection(),
		limiters:  make(map[string]rateLimiter),
	}
	srv.handler = srv.securityHeaders(srv.routes())
	return srv, nil
}

func (s *Server) Close() error {
	return s.store.Close()
}

func (s *Server) Handler() http.Handler {
	return s.handler
}

func (s *Server) Run(ctx context.Context) error {
	httpServer := &http.Server{
		Addr:              s.cfg.Addr,
		Handler:           s.handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	err := httpServer.ListenAndServe()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		if s.isHTTPS(r) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		csp := "default-src 'self'; " +
			"script-src 'self' 'unsafe-inline'; " +
			"style-src 'self' 'unsafe-inline'; " +
			"img-src 'self' data:; " +
			"font-src 'self' data:; " +
			"frame-ancestors 'none'; " +
			"form-action 'self';"
		w.Header().Set("Content-Security-Policy", csp)

		next.ServeHTTP(w, r)
	})
}

func (s *Server) isHTTPS(r *http.Request) bool {
	return r.TLS != nil
}

type rateLimiter struct {
	attempts int
	lastSeen time.Time
}

func (s *Server) limitLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.allowLoginAttempt(s.getRemoteIP(r), time.Now()) {
			s.renderWithStatus(w, http.StatusTooManyRequests, "login.gohtml", loginPageData{Error: "Too many login attempts. Please wait a minute."})
			return
		}

		next(w, r)
	}
}

func (s *Server) allowLoginAttempt(ip string, now time.Time) bool {
	if ip == "" {
		ip = "unknown"
	}

	s.limitersMu.Lock()
	defer s.limitersMu.Unlock()

	s.pruneLimitersLocked(now)

	rl := s.limiters[ip]
	if now.Sub(rl.lastSeen) > loginAttemptWindow {
		rl.attempts = 0
	}
	rl.lastSeen = now
	rl.attempts++
	s.limiters[ip] = rl

	return rl.attempts <= loginAttemptLimit
}

func (s *Server) pruneLimitersLocked(now time.Time) {
	for ip, rl := range s.limiters {
		if now.Sub(rl.lastSeen) > loginLimiterMaxIdle {
			delete(s.limiters, ip)
		}
	}
}

func (s *Server) getRemoteIP(r *http.Request) string {
	addr := strings.TrimSpace(r.RemoteAddr)
	if addr == "" {
		return ""
	}

	host, _, err := net.SplitHostPort(addr)
	if err == nil {
		return host
	}
	return addr
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(web.AssetFS())))
	mux.HandleFunc("GET /favicon.ico", s.handleFaviconICO)
	mux.HandleFunc("GET /favicon-16x16.png", s.handleFavicon16)
	mux.HandleFunc("GET /favicon-32x32.png", s.handleFavicon32)

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.Handle("POST /login", s.cop.Handler(s.limitLogin(s.handleLoginPost)))

	mux.Handle("GET /", s.withSession(s.handleDashboard))
	mux.Handle("POST /logout", s.cop.Handler(s.withSession(s.handleLogout)))

	mux.Handle("GET /tables/{table}", s.withSession(s.handleTablePanel))
	mux.Handle("GET /tables/{table}/rows", s.withSession(s.handleTableRows))
	mux.Handle("GET /tables/{table}/form", s.withSession(s.handleTableForm))
	mux.Handle("POST /tables/{table}/save", s.cop.Handler(s.withSession(s.handleTableSave)))
	mux.Handle("DELETE /tables/{table}/row/{id}", s.cop.Handler(s.withSession(s.handleTableDelete)))
	mux.Handle("POST /tables/{table}/import", s.cop.Handler(s.withSession(s.handleTableImport)))

	mux.Handle("GET /api/me", s.withSession(s.handleAPIMe))
	mux.Handle("GET /api/tables/{table}", s.withSession(s.handleAPITableList))
	mux.Handle("GET /api/tables/{table}/{id}", s.withSession(s.handleAPITableGet))
	mux.Handle("POST /api/tables/{table}", s.cop.Handler(s.withSession(s.handleAPITableCreate)))
	mux.Handle("PUT /api/tables/{table}/{id}", s.cop.Handler(s.withSession(s.handleAPITableUpdate)))
	mux.Handle("DELETE /api/tables/{table}/{id}", s.cop.Handler(s.withSession(s.handleAPITableDelete)))

	return mux
}

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.sessionFromRequest(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, "login.gohtml", loginPageData{})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.renderWithStatus(w, http.StatusBadRequest, "login.gohtml", loginPageData{Error: "Invalid login request."})
		return
	}

	loginName := strings.TrimSpace(r.FormValue("login_name"))
	password := r.FormValue("password")

	user, err := s.store.AuthenticateUser(r.Context(), loginName)
	if err != nil {
		s.renderWithStatus(w, http.StatusUnauthorized, "login.gohtml", loginPageData{Error: "Invalid login credentials."})
		return
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, password)
	if err != nil || !ok {
		s.renderWithStatus(w, http.StatusUnauthorized, "login.gohtml", loginPageData{Error: "Invalid login credentials."})
		return
	}

	session, err := s.sessions.Create(user.ID, user.LoginName, user.Role)
	if errors.Is(err, auth.ErrSessionLimit) {
		s.renderWithStatus(w, http.StatusForbidden, "login.gohtml", loginPageData{Error: "Session limit reached. Wait for an active session to expire or log out first."})
		return
	}
	if err != nil {
		s.renderWithStatus(w, http.StatusInternalServerError, "login.gohtml", loginPageData{Error: "Unable to create session."})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session.Token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.isHTTPS(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	s.sessions.Delete(principal.Token)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.isHTTPS(r),
	})

	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	tables := s.store.TablesForRole(principal.Role)
	if len(tables) == 0 {
		http.Error(w, "no tables available for this role", http.StatusForbidden)
		return
	}

	s.render(w, "dashboard.gohtml", dashboardPageData{
		User:         principal,
		Tables:       tables,
		DefaultTable: tables[0].Name,
	})
}

func (s *Server) handleTablePanel(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, false)
	if !ok {
		return
	}

	parentCtx, status, err := s.resolveParentContext(
		r.Context(),
		principal.Role,
		table,
		strings.TrimSpace(r.URL.Query().Get("parent_table")),
		strings.TrimSpace(r.URL.Query().Get("parent_id")),
		strings.TrimSpace(r.URL.Query().Get("parent_field")),
	)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	limit := viewportLimit(r)
	sortColumn := table.SortColumn(r.URL.Query().Get("sort"))
	desc := parseBool(r.URL.Query().Get("desc"))

	listOptions := store.ListOptions{
		Sort:  sortColumn,
		Desc:  desc,
		Limit: limit,
	}
	if parentCtx != nil {
		listOptions.Filter = map[string]any{parentCtx.Field: parentCtx.ParsedID}
	}

	result, err := s.store.List(r.Context(), table.Name, listOptions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := tablePanelData{
		User:      principal,
		Table:     table,
		NavTable:  table.Name,
		Headers:   buildHeaders(table, parentCtx, sortColumn, desc),
		Rows:      nil,
		CanWrite:  table.CanWrite(principal.Role),
		Sort:      sortColumn,
		Desc:      desc,
		Limit:     limit,
		HasMore:   result.HasMore,
		RowCount:  len(result.Rows),
		CanImport: table.ImportEnabled && table.CanWrite(principal.Role),
	}
	rows, err := s.buildRows(r.Context(), table, parentCtx, result.Rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	data.Rows = rows
	if parentCtx != nil {
		parentView, err := s.buildParentContextView(r.Context(), parentCtx, principal.Role)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		data.Parent = &parentView
		data.NavTable = parentCtx.Table.Name
	}
	if table.Subtable != nil {
		childTable, ok := s.store.Table(table.Subtable.Table)
		if ok && childTable.CanRead(principal.Role) {
			data.ChildTable = childTable.Name
			data.ChildField = table.Subtable.ForeignKey
		}
	}
	s.render(w, "table_panel.gohtml", data)
}

func (s *Server) handleTableRows(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, false)
	if !ok {
		return
	}

	parentCtx, status, err := s.resolveParentContext(
		r.Context(),
		principal.Role,
		table,
		strings.TrimSpace(r.URL.Query().Get("parent_table")),
		strings.TrimSpace(r.URL.Query().Get("parent_id")),
		strings.TrimSpace(r.URL.Query().Get("parent_field")),
	)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	listOptions := store.ListOptions{
		Sort:   table.SortColumn(r.URL.Query().Get("sort")),
		Desc:   parseBool(r.URL.Query().Get("desc")),
		Limit:  viewportLimit(r),
		Offset: offset,
	}
	if parentCtx != nil {
		listOptions.Filter = map[string]any{parentCtx.Field: parentCtx.ParsedID}
	}

	result, err := s.store.List(r.Context(), table.Name, listOptions)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Has-More", strconv.FormatBool(result.HasMore))
	rows, err := s.buildRows(r.Context(), table, parentCtx, result.Rows)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "table_rows.gohtml", rows)
}

func (s *Server) handleTableForm(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
		return
	}

	parentCtx, status, err := s.resolveParentContext(
		r.Context(),
		principal.Role,
		table,
		strings.TrimSpace(r.URL.Query().Get("parent_table")),
		strings.TrimSpace(r.URL.Query().Get("parent_id")),
		strings.TrimSpace(r.URL.Query().Get("parent_field")),
	)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	rowID := strings.TrimSpace(r.URL.Query().Get("id"))
	row := map[string]any{}
	if rowID != "" {
		record, err := s.store.Get(r.Context(), table.Name, rowID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "row not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		row = record
	}

	data, err := s.buildFormData(r.Context(), principal, table, rowID, row, parentCtx, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "modal_form.gohtml", data)
}

func (s *Server) sanitizeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if strings.Contains(msg, "UNIQUE constraint failed") {
		return "A record with this information already exists."
	}
	if strings.Contains(msg, "FOREIGN KEY constraint failed") {
		return "This record is being used by another table and cannot be changed or deleted."
	}
	return "An error occurred while saving the record."
}

func (s *Server) handleTableSave(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
		return
	}

	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(16 << 20); err != nil {
			s.renderFormError(w, r, principal, table, "", nil, nil, "Invalid form payload.")
			return
		}
	} else {
		if err := r.ParseForm(); err != nil {
			s.renderFormError(w, r, principal, table, "", nil, nil, "Invalid form payload.")
			return
		}
	}

	parentCtx, status, err := s.resolveParentContext(
		r.Context(),
		principal.Role,
		table,
		strings.TrimSpace(r.FormValue("parent_table")),
		strings.TrimSpace(r.FormValue("parent_id")),
		strings.TrimSpace(r.FormValue("parent_field")),
	)
	if err != nil {
		http.Error(w, err.Error(), status)
		return
	}

	rowID := strings.TrimSpace(r.FormValue("row_id"))
	values, err := s.parseFormValues(r, table, rowID == "")
	if err != nil {
		s.renderFormError(w, r, principal, table, rowID, values, parentCtx, err.Error())
		return
	}
	s.applyAutomaticUserID(table, principal, rowID == "", values)
	if parentCtx != nil {
		values[parentCtx.Field] = parentCtx.ParsedID
	}

	if rowID == "" {
		if _, err := s.store.Insert(r.Context(), table.Name, values); err != nil {
			s.renderFormError(w, r, principal, table, rowID, values, parentCtx, s.sanitizeError(err))
			return
		}
	} else {
		if err := s.store.Update(r.Context(), table.Name, rowID, values); err != nil {
			s.renderFormError(w, r, principal, table, rowID, values, parentCtx, s.sanitizeError(err))
			return
		}
	}

	s.sendHTMXSuccess(w, "Saved record.")
}

func (s *Server) handleTableDelete(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if table.Name == "users" {
		record, err := s.store.Get(r.Context(), table.Name, id)
		if err != nil {
			http.Error(w, "row not found", http.StatusNotFound)
			return
		}
		if fmt.Sprint(record["usr_role"]) == "admin" {
			admins, err := s.store.CountAdmins(r.Context())
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if admins <= 1 {
				s.renderInlineError(w, http.StatusConflict, "Deleting the last admin user is blocked.")
				return
			}
		}
	}

	if err := s.store.Delete(r.Context(), table.Name, id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.sendHTMXDeleteSuccess(w, table.Name, id, "Deleted record.")
}

func (s *Server) handleTableImport(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
		return
	}

	file, _, err := r.FormFile("csv_file")
	if err != nil {
		s.renderInlineError(w, http.StatusBadRequest, "Select a CSV file to import.")
		return
	}
	defer file.Close()

	imported, err := s.store.ImportCSV(r.Context(), table.Name, file, func(field store.Field, raw string) (any, error) {
		value, err := store.ParseFieldValue(field, raw)
		if err != nil {
			return nil, err
		}
		if field.Kind == store.KindPassword {
			if strings.TrimSpace(raw) == "" {
				return nil, nil
			}
			return auth.HashPassword(raw)
		}
		return value, nil
	})
	if err != nil {
		s.renderInlineError(w, http.StatusBadRequest, err.Error())
		return
	}

	s.sendHTMXSuccess(w, fmt.Sprintf("Imported %d rows.", imported))
}

func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	s.writeJSON(w, http.StatusOK, apiResponse{
		User: principal.LoginName,
		Role: principal.Role,
	})
}

func (s *Server) handleAPITableList(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTableAPI(w, r, principal.Role, false)
	if !ok {
		return
	}

	result, err := s.store.List(r.Context(), table.Name, store.ListOptions{
		Sort:   table.SortColumn(r.URL.Query().Get("sort")),
		Desc:   parseBool(r.URL.Query().Get("desc")),
		Limit:  viewportLimit(r),
		Offset: intValue(r.URL.Query().Get("offset")),
	})
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, apiResponse{
		Table:   table.Name,
		Rows:    result.Rows,
		HasMore: result.HasMore,
	})
}

func (s *Server) handleAPITableGet(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTableAPI(w, r, principal.Role, false)
	if !ok {
		return
	}

	row, err := s.store.Get(r.Context(), table.Name, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.writeJSON(w, http.StatusNotFound, apiResponse{Error: "row not found"})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, apiResponse{Error: err.Error()})
		return
	}

	s.writeJSON(w, http.StatusOK, apiResponse{
		Table: table.Name,
		Row:   row,
	})
}

func (s *Server) handleAPITableCreate(w http.ResponseWriter, r *http.Request) {
	s.handleAPITableWrite(w, r, true)
}

func (s *Server) handleAPITableUpdate(w http.ResponseWriter, r *http.Request) {
	s.handleAPITableWrite(w, r, false)
}

func (s *Server) handleAPITableWrite(w http.ResponseWriter, r *http.Request, create bool) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTableAPI(w, r, principal.Role, true)
	if !ok {
		return
	}

	var payload map[string]any
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiResponse{Error: "invalid JSON body"})
		return
	}

	values, err := s.parseAPIValues(table, payload, create)
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	s.applyAutomaticUserID(table, principal, create, values)

	if create {
		id, err := s.store.Insert(r.Context(), table.Name, values)
		if err != nil {
			s.writeJSON(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
			return
		}
		row, _ := s.store.Get(r.Context(), table.Name, strconv.FormatInt(id, 10))
		s.writeJSON(w, http.StatusCreated, apiResponse{Table: table.Name, Row: row})
		return
	}

	id := r.PathValue("id")
	if err := s.store.Update(r.Context(), table.Name, id, values); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	row, _ := s.store.Get(r.Context(), table.Name, id)
	s.writeJSON(w, http.StatusOK, apiResponse{Table: table.Name, Row: row})
}

func (s *Server) handleAPITableDelete(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTableAPI(w, r, principal.Role, true)
	if !ok {
		return
	}

	id := r.PathValue("id")
	if table.Name == "users" {
		record, err := s.store.Get(r.Context(), table.Name, id)
		if err == nil && fmt.Sprint(record["usr_role"]) == "admin" {
			admins, err := s.store.CountAdmins(r.Context())
			if err == nil && admins <= 1 {
				s.writeJSON(w, http.StatusConflict, apiResponse{Error: "deleting the last admin user is blocked"})
				return
			}
		}
	}

	if err := s.store.Delete(r.Context(), table.Name, id); err != nil {
		s.writeJSON(w, http.StatusBadRequest, apiResponse{Error: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) parseAPIValues(table store.TableDef, payload map[string]any, create bool) (map[string]any, error) {
	values := make(map[string]any)
	for _, field := range table.EditableFields() {
		if isAutomaticUserField(table, field) {
			continue
		}
		rawValue, ok := payload[field.Column]
		if !ok {
			if field.Required && create && field.Kind != store.KindBlob {
				return nil, fmt.Errorf("%s is required", field.Label)
			}
			continue
		}

		switch field.Kind {
		case store.KindInteger, store.KindForeign:
			switch typed := rawValue.(type) {
			case float64:
				values[field.Column] = int64(typed)
			case string:
				parsed, err := store.ParseFieldValue(field, typed)
				if err != nil {
					return nil, fmt.Errorf("parse %s: %w", field.Column, err)
				}
				if parsed != nil {
					values[field.Column] = parsed
				}
			}
		case store.KindReal:
			switch typed := rawValue.(type) {
			case float64:
				values[field.Column] = typed
			case string:
				parsed, err := store.ParseFieldValue(field, typed)
				if err != nil {
					return nil, fmt.Errorf("parse %s: %w", field.Column, err)
				}
				if parsed != nil {
					values[field.Column] = parsed
				}
			}
		case store.KindPassword:
			password, _ := rawValue.(string)
			if create && strings.TrimSpace(password) == "" {
				return nil, fmt.Errorf("%s is required", field.Label)
			}
			if strings.TrimSpace(password) != "" {
				hash, err := auth.HashPassword(password)
				if err != nil {
					return nil, err
				}
				values[field.Column] = hash
			}
		default:
			if text, ok := rawValue.(string); ok {
				parsed, err := store.ParseFieldValue(field, text)
				if err != nil {
					return nil, fmt.Errorf("parse %s: %w", field.Column, err)
				}
				if parsed != nil {
					values[field.Column] = parsed
				}
			}
		}
	}
	return values, nil
}

func (s *Server) parseFormValues(r *http.Request, table store.TableDef, create bool) (map[string]any, error) {
	values := make(map[string]any)
	for _, field := range table.EditableFields() {
		if isAutomaticUserField(table, field) {
			continue
		}
		switch field.Kind {
		case store.KindBlob:
			file, _, err := r.FormFile(field.Column)
			if err != nil {
				if create && field.Required {
					return nil, fmt.Errorf("%s is required", field.Label)
				}
				continue
			}
			content, err := io.ReadAll(file)
			_ = file.Close()
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", field.Label, err)
			}
			if len(content) > 0 {
				values[field.Column] = content
			}
		case store.KindPassword:
			raw := r.FormValue(field.Column)
			if create && strings.TrimSpace(raw) == "" {
				return nil, fmt.Errorf("%s is required", field.Label)
			}
			if strings.TrimSpace(raw) == "" {
				continue
			}
			hash, err := auth.HashPassword(raw)
			if err != nil {
				return nil, fmt.Errorf("hash password: %w", err)
			}
			values[field.Column] = hash
		default:
			value, err := store.ParseFieldValue(field, r.FormValue(field.Column))
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", field.Label, err)
			}
			if field.Required {
				switch typed := value.(type) {
				case nil:
					return nil, fmt.Errorf("%s is required", field.Label)
				case string:
					if strings.TrimSpace(typed) == "" {
						return nil, fmt.Errorf("%s is required", field.Label)
					}
				}
			}
			if value != nil {
				values[field.Column] = value
			}
		}
	}
	return values, nil
}

func (s *Server) buildFormData(ctx context.Context, principal Principal, table store.TableDef, rowID string, row map[string]any, parentCtx *parentContext, message string) (formData, error) {
	fields := make([]formFieldView, 0, len(table.EditableFields()))
	firstField := true
	for _, field := range table.EditableFields() {
		options := []store.Option{}
		if field.Kind == store.KindForeign {
			refOptions, err := s.store.ReferenceOptions(ctx, field.RefTable)
			if err != nil {
				return formData{}, err
			}
			options = refOptions
		}
		if field.Kind == store.KindEnum || field.Kind == store.KindStatus {
			options = make([]store.Option, 0, len(field.Options)+1)
			options = append(options, store.Option{Value: "", Label: ""})
			for _, option := range field.Options {
				options = append(options, store.Option{Value: option, Label: option})
			}
		}

		value := ""
		if raw, ok := row[field.Column]; ok && field.Kind != store.KindPassword {
			value = store.DisplayValue(field, raw)
		} else if parentCtx != nil && field.Column == parentCtx.Field {
			value = parentCtx.RowID
		} else if rowID == "" && field.Column == "usr_id" && table.Name != "users" {
			value = strconv.FormatInt(principal.UserID, 10)
		} else if rowID == "" && field.Kind == store.KindStatus {
			value = "Draft"
		}

		help := ""
		hasValue := value != ""
		if field.Kind == store.KindBlob && rowID != "" && hasValue {
			help = "Existing file is kept unless a new file is uploaded."
			value = ""
		}

		visible := true
		if parentCtx != nil && field.Column == parentCtx.Field {
			visible = false
		}
		if isAutomaticUserField(table, field) {
			visible = false
		}

		autofocus := visible && firstField
		if visible {
			firstField = false
		}

		fields = append(fields, formFieldView{
			Column:      field.Column,
			Label:       field.Label,
			Kind:        string(field.Kind),
			Value:       value,
			Required:    field.Required && !(field.Kind == store.KindPassword && rowID != ""),
			Options:     options,
			Accept:      field.Accept,
			HasValue:    hasValue,
			Help:        help,
			Visible:     visible,
			Rows:        textareaRows(field.Kind),
			Autofocus:   autofocus,
			Placeholder: field.Placeholder,
		})
	}

	var parentView *parentContextView
	if parentCtx != nil {
		view, err := s.buildParentContextView(ctx, parentCtx, principal.Role)
		if err != nil {
			return formData{}, err
		}
		parentView = &view
	}

	deleteSummary, err := s.buildDeleteSummary(ctx, table, rowID, row)
	if err != nil {
		return formData{}, err
	}

	return formData{
		Table:                table,
		User:                 principal,
		Parent:               parentView,
		Fields:               fields,
		RowID:                rowID,
		Error:                message,
		CanDelete:            rowID != "",
		SubmitPath:           "/tables/" + table.Name + "/save",
		DeletePath:           "/tables/" + table.Name + "/row/" + rowID,
		DeleteConfirmMessage: buildDeletePrompt(table.Label, deleteSummary),
	}, nil
}

func (s *Server) renderFormError(w http.ResponseWriter, r *http.Request, principal Principal, table store.TableDef, rowID string, values map[string]any, parentCtx *parentContext, message string) {
	data, err := s.buildFormData(r.Context(), principal, table, rowID, values, parentCtx, message)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.renderWithStatus(w, http.StatusBadRequest, "modal_form.gohtml", data)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	if err := s.templates.Render(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *Server) renderWithStatus(w http.ResponseWriter, status int, name string, data any) {
	w.WriteHeader(status)
	s.render(w, name, data)
}

func (s *Server) renderInlineError(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	fmt.Fprintf(w, `<div class="rounded border border-red-300 bg-red-50 px-3 py-2 text-sm text-red-700">%s</div>`, html.EscapeString(message))
}

func (s *Server) sendHTMXSuccess(w http.ResponseWriter, message string) {
	w.Header().Set("HX-Trigger", fmt.Sprintf(`{"stockit:refresh-table":{},"stockit:close-modal":{},"stockit:toast":{"message":%q}}`, message))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) sendHTMXDeleteSuccess(w http.ResponseWriter, tableName, id, message string) {
	w.Header().Set("HX-Trigger", fmt.Sprintf(`{"stockit:record-deleted":{"table":%q,"id":%q},"stockit:close-modal":{},"stockit:toast":{"message":%q}}`, tableName, id, message))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *Server) withSession(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		session, ok := s.sessionFromRequest(r)
		if !ok {
			s.handleUnauthenticated(w, r)
			return
		}

		ctx := context.WithValue(r.Context(), principalKey, Principal{
			UserID:    session.UserID,
			LoginName: session.LoginName,
			Role:      session.Role,
			Token:     session.Token,
		})
		next(w, r.WithContext(ctx))
	})
}

func (s *Server) sessionFromRequest(r *http.Request) (*auth.Session, bool) {
	if token := bearerToken(r.Header.Get("Authorization")); token != "" {
		if session, ok := s.sessions.Get(token); ok {
			return session, true
		}
	}

	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || strings.TrimSpace(cookie.Value) == "" {
		return nil, false
	}
	return s.sessions.Get(cookie.Value)
}

func (s *Server) handleUnauthenticated(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		s.writeJSON(w, http.StatusUnauthorized, apiResponse{Error: "authentication required"})
		return
	}
	if r.Header.Get("HX-Request") != "" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) authorizeTable(w http.ResponseWriter, r *http.Request, role string, write bool) (store.TableDef, bool) {
	tableName := r.PathValue("table")
	table, ok := s.store.Table(tableName)
	if !ok {
		http.NotFound(w, r)
		return store.TableDef{}, false
	}
	if write && !table.CanWrite(role) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return store.TableDef{}, false
	}
	if !write && !table.CanRead(role) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return store.TableDef{}, false
	}
	return table, true
}

func (s *Server) authorizeTableAPI(w http.ResponseWriter, r *http.Request, role string, write bool) (store.TableDef, bool) {
	tableName := r.PathValue("table")
	table, ok := s.store.Table(tableName)
	if !ok {
		s.writeJSON(w, http.StatusNotFound, apiResponse{Error: "table unavailable"})
		return store.TableDef{}, false
	}
	if write && !table.CanWrite(role) {
		s.writeJSON(w, http.StatusForbidden, apiResponse{Error: "forbidden"})
		return store.TableDef{}, false
	}
	if !write && !table.CanRead(role) {
		s.writeJSON(w, http.StatusForbidden, apiResponse{Error: "forbidden"})
		return store.TableDef{}, false
	}
	return table, true
}

func (s *Server) handleFaviconICO(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/x-icon")
	_, _ = w.Write(web.FaviconICO())
}

func (s *Server) handleFavicon16(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(web.Favicon16())
}

func (s *Server) handleFavicon32(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(web.Favicon32())
}

func buildHeaders(table store.TableDef, parentCtx *parentContext, sort string, desc bool) []tableHeaderView {
	fields := visibleListFields(table, parentCtx)
	headers := make([]tableHeaderView, 0, len(fields))
	for _, field := range fields {
		headers = append(headers, tableHeaderView{
			Column: field.Column,
			Label:  field.Label,
			Active: field.Column == sort,
			Desc:   field.Column == sort && desc,
		})
	}
	return headers
}

func (s *Server) buildRows(ctx context.Context, table store.TableDef, parentCtx *parentContext, records []map[string]any) ([]tableRowView, error) {
	fields := visibleListFields(table, parentCtx)
	rows := make([]tableRowView, 0, len(records))
	foreignLabels := map[string]map[string]string{}
	for _, record := range records {
		cells := make([]string, 0, len(fields))
		for _, field := range fields {
			value, err := s.displayTableValue(ctx, field, record[field.Column], foreignLabels)
			if err != nil {
				return nil, err
			}
			cells = append(cells, value)
		}
		rowID := fmt.Sprint(record[table.PrimaryKey])
		rows = append(rows, tableRowView{
			ID:            rowID,
			Cells:         cells,
			DeleteConfirm: buildDeletePrompt(table.Label, buildDeleteSummaryFromCells(table, fields, rowID, cells)),
		})
	}
	return rows, nil
}

func visibleListFields(table store.TableDef, parentCtx *parentContext) []store.Field {
	fields := table.ListFields()
	if parentCtx == nil || parentCtx.Field == "" {
		return fields
	}

	visible := make([]store.Field, 0, len(fields))
	for _, field := range fields {
		if field.Column == parentCtx.Field {
			continue
		}
		visible = append(visible, field)
	}
	return visible
}

func (s *Server) buildParentContextView(ctx context.Context, parentCtx *parentContext, role string) (parentContextView, error) {
	view := parentContextView{
		TableName:  parentCtx.Table.Name,
		TableLabel: parentCtx.Table.Label,
		RowID:      parentCtx.RowID,
		Field:      parentCtx.Field,
		Label:      parentCtx.Label,
		Title:      parentCtx.Table.DisplayValue(parentCtx.Row),
		CanWrite:   parentCtx.Table.CanWrite(role),
	}

	for _, field := range parentCtx.Table.Fields {
		if field.Column == "usr_id" || field.Column == "created_at" || field.Kind == store.KindPassword || field.Kind == store.KindBlob {
			continue
		}

		raw, ok := parentCtx.Row[field.Column]
		if !ok || raw == nil {
			continue
		}

		value, err := s.displayContextValue(ctx, field, raw)
		if err != nil {
			return parentContextView{}, err
		}
		value = compactSummaryValue(value)
		if value == "" {
			continue
		}

		view.Summary = append(view.Summary, parentFieldView{
			Label: field.Label,
			Value: value,
		})
	}

	if len(view.Summary) == 0 && strings.TrimSpace(view.Title) != "" {
		view.Summary = append(view.Summary, parentFieldView{
			Label: parentCtx.Table.Label,
			Value: compactSummaryValue(view.Title),
		})
	}

	return view, nil
}

func (s *Server) displayContextValue(ctx context.Context, field store.Field, raw any) (string, error) {
	if field.Kind != store.KindForeign {
		return strings.TrimSpace(store.DisplayValue(field, raw)), nil
	}

	options, err := s.store.ReferenceOptions(ctx, field.RefTable)
	if err != nil {
		return "", err
	}
	current := strings.TrimSpace(store.DisplayValue(field, raw))
	for _, option := range options {
		if option.Value == current {
			return strings.TrimSpace(option.Label), nil
		}
	}

	return current, nil
}

func (s *Server) displayTableValue(ctx context.Context, field store.Field, raw any, foreignLabels map[string]map[string]string) (string, error) {
	if field.Kind != store.KindForeign {
		return strings.TrimSpace(store.DisplayValue(field, raw)), nil
	}

	current := strings.TrimSpace(store.DisplayValue(field, raw))
	if current == "" {
		return "", nil
	}

	labels, ok := foreignLabels[field.RefTable]
	if !ok {
		options, err := s.store.ReferenceOptions(ctx, field.RefTable)
		if err != nil {
			return "", err
		}
		labels = make(map[string]string, len(options))
		for _, option := range options {
			if option.Value == "" {
				continue
			}
			labels[option.Value] = strings.TrimSpace(option.Label)
		}
		foreignLabels[field.RefTable] = labels
	}

	if label := labels[current]; label != "" {
		return compactReferenceLabel(field.RefTable, label), nil
	}
	return current, nil
}

func (s *Server) buildDeleteSummary(ctx context.Context, table store.TableDef, rowID string, row map[string]any) (string, error) {
	if rowID == "" || len(row) == 0 {
		return "", nil
	}

	fields := table.ListFields()
	foreignLabels := map[string]map[string]string{}
	cells := make([]string, 0, len(fields))
	for _, field := range fields {
		value, err := s.displayTableValue(ctx, field, row[field.Column], foreignLabels)
		if err != nil {
			return "", err
		}
		cells = append(cells, value)
	}
	return buildDeleteSummaryFromCells(table, fields, rowID, cells), nil
}

func buildDeleteSummaryFromCells(table store.TableDef, fields []store.Field, rowID string, cells []string) string {
	parts := make([]string, 0, 3)
	for index, field := range fields {
		if index >= len(cells) {
			break
		}
		if field.Column == table.PrimaryKey || field.Column == "created_at" {
			continue
		}

		value := compactSummaryValue(cells[index])
		if value == "" {
			continue
		}
		parts = append(parts, value)
		if len(parts) == 3 {
			break
		}
	}

	if len(parts) > 0 {
		return strings.Join(parts, " | ")
	}
	if rowID != "" {
		return "#" + rowID
	}
	return ""
}

func buildDeletePrompt(tableLabel, summary string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return fmt.Sprintf("Delete record from %s?", tableLabel)
	}
	return fmt.Sprintf("Delete record from %s?\n%s", tableLabel, summary)
}

func compactReferenceLabel(refTable, label string) string {
	if refTable != "users" {
		return label
	}

	parts := strings.Split(label, " | ")
	if len(parts) >= 2 {
		return strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(label)
}

func compactSummaryValue(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	if value == "" {
		return ""
	}

	runes := []rune(value)
	if len(runes) <= 72 {
		return value
	}
	return string(runes[:69]) + "..."
}

func isAutomaticUserField(table store.TableDef, field store.Field) bool {
	return table.Name != "users" && field.Column == "usr_id"
}

func (s *Server) applyAutomaticUserID(table store.TableDef, principal Principal, create bool, values map[string]any) {
	if table.Name == "users" {
		return
	}
	if create {
		if _, ok := table.Field("usr_id"); ok {
			values["usr_id"] = principal.UserID
		}
		return
	}
	delete(values, "usr_id")
}

func (s *Server) resolveParentContext(ctx context.Context, role string, table store.TableDef, parentTableName, parentID, parentField string) (*parentContext, int, error) {
	if strings.TrimSpace(parentID) == "" {
		return nil, 0, nil
	}
	if !table.IsSubtable() {
		return nil, http.StatusBadRequest, fmt.Errorf("parent context is not supported for %s", table.Label)
	}

	if parentTableName == "" {
		parentTableName = table.ParentTable
	}
	if parentField == "" {
		parentField = table.ParentField
	}
	if parentTableName != table.ParentTable || parentField != table.ParentField {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid parent context for %s", table.Label)
	}

	filterField, ok := table.Field(parentField)
	if !ok {
		return nil, http.StatusInternalServerError, fmt.Errorf("unknown parent field %q for %s", parentField, table.Name)
	}
	parsedParentID, err := store.ParseFieldValue(filterField, parentID)
	if err != nil || parsedParentID == nil {
		return nil, http.StatusBadRequest, fmt.Errorf("invalid parent id")
	}

	parentTable, ok := s.store.Table(parentTableName)
	if !ok {
		return nil, http.StatusNotFound, fmt.Errorf("parent table unavailable")
	}
	if !parentTable.CanRead(role) {
		return nil, http.StatusForbidden, fmt.Errorf("forbidden")
	}

	parentRow, err := s.store.Get(ctx, parentTable.Name, parentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, http.StatusNotFound, fmt.Errorf("parent row not found")
		}
		return nil, http.StatusInternalServerError, err
	}

	label := strings.TrimSpace(table.ParentLabel)
	if label == "" {
		label = "Selected " + parentTable.Label
	}

	return &parentContext{
		Table:    parentTable,
		Row:      parentRow,
		RowID:    parentID,
		Field:    parentField,
		Label:    label,
		ParsedID: parsedParentID,
	}, 0, nil
}

func principalFromContext(ctx context.Context) Principal {
	principal, _ := ctx.Value(principalKey).(Principal)
	return principal
}

func viewportLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 20 {
		return 30
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intValue(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func bearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func textareaRows(kind store.FieldKind) int {
	if kind == store.KindTextarea {
		return 1
	}
	return 0
}
