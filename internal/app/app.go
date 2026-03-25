package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
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
	cfg       Config
	store     *store.Store
	sessions  *auth.Manager
	templates *web.Templates
	cop       *http.CrossOriginProtection
	handler   http.Handler
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
	User      Principal
	Table     store.TableDef
	Headers   []tableHeaderView
	Rows      []tableRowView
	CanWrite  bool
	Sort      string
	Desc      bool
	Limit     int
	HasMore   bool
	RowCount  int
	Message   string
	CanImport bool
}

type tableHeaderView struct {
	Column string
	Label  string
	Active bool
	Desc   bool
}

type tableRowView struct {
	ID    string
	Cells []string
}

type formData struct {
	Table      store.TableDef
	User       Principal
	Fields     []formFieldView
	RowID      string
	Error      string
	CanDelete  bool
	SubmitPath string
	DeletePath string
}

type formFieldView struct {
	Column      string
	Label       string
	Kind        store.FieldKind
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
	}
	srv.handler = srv.routes()
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

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(web.AssetFS())))
	mux.HandleFunc("GET /favicon.ico", s.handleFaviconICO)
	mux.HandleFunc("GET /favicon-16x16.png", s.handleFavicon16)
	mux.HandleFunc("GET /favicon-32x32.png", s.handleFavicon32)

	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.Handle("POST /login", s.cop.Handler(http.HandlerFunc(s.handleLoginPost)))

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
		Secure:   r.TLS != nil,
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
		Secure:   r.TLS != nil,
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

	limit := viewportLimit(r)
	sortColumn := table.SortColumn(r.URL.Query().Get("sort"))
	desc := parseBool(r.URL.Query().Get("desc"))

	result, err := s.store.List(r.Context(), table.Name, store.ListOptions{
		Sort:  sortColumn,
		Desc:  desc,
		Limit: limit,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	data := tablePanelData{
		User:      principal,
		Table:     table,
		Headers:   buildHeaders(table, sortColumn, desc),
		Rows:      buildRows(table, result.Rows),
		CanWrite:  table.CanWrite(principal.Role),
		Sort:      sortColumn,
		Desc:      desc,
		Limit:     limit,
		HasMore:   result.HasMore,
		RowCount:  len(result.Rows),
		CanImport: table.ImportEnabled && table.CanWrite(principal.Role),
	}
	s.render(w, "table_panel.gohtml", data)
}

func (s *Server) handleTableRows(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, false)
	if !ok {
		return
	}

	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	result, err := s.store.List(r.Context(), table.Name, store.ListOptions{
		Sort:   table.SortColumn(r.URL.Query().Get("sort")),
		Desc:   parseBool(r.URL.Query().Get("desc")),
		Limit:  viewportLimit(r),
		Offset: offset,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("X-Has-More", strconv.FormatBool(result.HasMore))
	s.render(w, "table_rows.gohtml", buildRows(table, result.Rows))
}

func (s *Server) handleTableForm(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
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

	data, err := s.buildFormData(r.Context(), principal, table, rowID, row, "")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.render(w, "modal_form.gohtml", data)
}

func (s *Server) handleTableSave(w http.ResponseWriter, r *http.Request) {
	principal := principalFromContext(r.Context())
	table, ok := s.authorizeTable(w, r, principal.Role, true)
	if !ok {
		return
	}

	if err := r.ParseMultipartForm(16 << 20); err != nil {
		s.renderFormError(w, r, principal, table, "", nil, "Invalid form payload.")
		return
	}

	rowID := strings.TrimSpace(r.FormValue("row_id"))
	values, err := s.parseFormValues(r, table, rowID == "")
	if err != nil {
		s.renderFormError(w, r, principal, table, rowID, values, err.Error())
		return
	}

	if rowID == "" {
		if _, err := s.store.Insert(r.Context(), table.Name, values); err != nil {
			s.renderFormError(w, r, principal, table, rowID, values, err.Error())
			return
		}
	} else {
		if err := s.store.Update(r.Context(), table.Name, rowID, values); err != nil {
			s.renderFormError(w, r, principal, table, rowID, values, err.Error())
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
	s.sendHTMXSuccess(w, "Deleted record.")
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
				values[field.Column] = strings.TrimSpace(text)
			}
		}
	}
	return values, nil
}

func (s *Server) parseFormValues(r *http.Request, table store.TableDef, create bool) (map[string]any, error) {
	values := make(map[string]any)
	for _, field := range table.EditableFields() {
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

func (s *Server) buildFormData(ctx context.Context, principal Principal, table store.TableDef, rowID string, row map[string]any, message string) (formData, error) {
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

		fields = append(fields, formFieldView{
			Column:      field.Column,
			Label:       field.Label,
			Kind:        field.Kind,
			Value:       value,
			Required:    field.Required && !(field.Kind == store.KindPassword && rowID != ""),
			Options:     options,
			Accept:      field.Accept,
			HasValue:    hasValue,
			Help:        help,
			Visible:     true,
			Rows:        textareaRows(field.Kind),
			Autofocus:   firstField,
			Placeholder: field.Placeholder,
		})
		firstField = false
	}

	return formData{
		Table:      table,
		User:       principal,
		Fields:     fields,
		RowID:      rowID,
		Error:      message,
		CanDelete:  rowID != "",
		SubmitPath: "/tables/" + table.Name + "/save",
		DeletePath: "/tables/" + table.Name + "/row/" + rowID,
	}, nil
}

func (s *Server) renderFormError(w http.ResponseWriter, r *http.Request, principal Principal, table store.TableDef, rowID string, values map[string]any, message string) {
	data, err := s.buildFormData(r.Context(), principal, table, rowID, values, message)
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

func buildHeaders(table store.TableDef, sort string, desc bool) []tableHeaderView {
	headers := make([]tableHeaderView, 0, len(table.ListFields()))
	for _, field := range table.ListFields() {
		headers = append(headers, tableHeaderView{
			Column: field.Column,
			Label:  field.Label,
			Active: field.Column == sort,
			Desc:   field.Column == sort && desc,
		})
	}
	return headers
}

func buildRows(table store.TableDef, records []map[string]any) []tableRowView {
	fields := table.ListFields()
	rows := make([]tableRowView, 0, len(records))
	for _, record := range records {
		cells := make([]string, 0, len(fields))
		for _, field := range fields {
			cells = append(cells, store.DisplayValue(field, record[field.Column]))
		}
		rows = append(rows, tableRowView{
			ID:    fmt.Sprint(record[table.PrimaryKey]),
			Cells: cells,
		})
	}
	return rows
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
		return 3
	}
	return 0
}
