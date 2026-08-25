package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"stockit/internal/auth"
	"stockit/internal/store"
)

type apiLoginRequest struct {
	LoginName string `json:"login_name"`
	Password  string `json:"password"`
}

type apiLoginResponse struct {
	Token                  string `json:"token"`
	TokenType              string `json:"token_type"`
	User                   string `json:"user"`
	Role                   string `json:"role"`
	SessionIdleTimeoutSecs int64  `json:"session_idle_timeout_seconds"`
}

type apiTableSummary struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	PrimaryKey  string `json:"primary_key"`
	DefaultSort string `json:"default_sort"`
	CanWrite    bool   `json:"can_write"`
	IsSubtable  bool   `json:"is_subtable"`
	ParentTable string `json:"parent_table,omitempty"`
	ParentField string `json:"parent_field,omitempty"`
}

type apiTableField struct {
	Column      string   `json:"column"`
	Label       string   `json:"label"`
	Kind        string   `json:"kind"`
	Required    bool     `json:"required"`
	Editable    bool     `json:"editable"`
	List        bool     `json:"list"`
	Sortable    bool     `json:"sortable"`
	Options     []string `json:"options,omitempty"`
	RefTable    string   `json:"ref_table,omitempty"`
	Accept      string   `json:"accept,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
}

type apiTableSchema struct {
	Name          string          `json:"name"`
	Label         string          `json:"label"`
	PrimaryKey    string          `json:"primary_key"`
	TitleColumn   string          `json:"title_column"`
	DefaultSort   string          `json:"default_sort"`
	DefaultDesc   bool            `json:"default_desc"`
	CanWrite      bool            `json:"can_write"`
	ImportEnabled bool            `json:"import_enabled"`
	IsSubtable    bool            `json:"is_subtable"`
	ParentTable   string          `json:"parent_table,omitempty"`
	ParentField   string          `json:"parent_field,omitempty"`
	ParentLabel   string          `json:"parent_label,omitempty"`
	Fields        []apiTableField `json:"fields"`
}

type apiTableListResponse struct {
	Table   string           `json:"table"`
	Rows    []map[string]any `json:"rows"`
	HasMore bool             `json:"has_more"`
}

type apiTableRowResponse struct {
	Table string         `json:"table"`
	ID    string         `json:"id"`
	Row   map[string]any `json:"row"`
}

type apiTableDeleteResponse struct {
	Table   string `json:"table"`
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
}

type apiTableImportResponse struct {
	Table    string `json:"table"`
	Imported int    `json:"imported"`
}

type apiErrorResponse struct {
	Error string `json:"error"`
}

type apiMeResponse struct {
	User string `json:"user"`
	Role string `json:"role"`
	// ApprovalLimitMinor is the largest purchase order this user may approve, in
	// integer minor units. Zero means no approval authority.
	ApprovalLimitMinor int64 `json:"approval_limit_minor"`
}

type apiUserNamesResponse struct {
	Users []store.UserName `json:"users"`
}

type apiTableListEnvelope struct {
	Tables []apiTableSummary `json:"tables"`
}

type apiTableSchemaEnvelope struct {
	Table apiTableSchema `json:"table"`
}

type tableListInput struct {
	Sort        string
	Desc        bool
	Limit       int
	Offset      int
	ParentField string
	ParentID    string
	// Equals, From and To are raw column filters keyed by column name; From and
	// To bound a column inclusively, which is how callers page a date range.
	Equals map[string]string
	From   map[string]string
	To     map[string]string
	// Search is a case-insensitive substring matched against the table's listed
	// text columns.
	Search string
}

func (s *Server) roleHasAnyWritableTable(role string) bool {
	for _, table := range s.store.TablesForRole(role) {
		if table.CanWrite(role) {
			return true
		}
	}
	return false
}

func (s *Server) listAPITables(principal Principal) []apiTableSummary {
	tables := s.store.TablesForRole(principal.Role)
	result := make([]apiTableSummary, 0, len(tables))
	for _, table := range tables {
		result = append(result, apiTableSummary{
			Name:        table.Name,
			Label:       table.Label,
			PrimaryKey:  table.PrimaryKey,
			DefaultSort: table.DefaultSort,
			CanWrite:    table.CanWrite(principal.Role),
			IsSubtable:  table.IsSubtable(),
			ParentTable: table.ParentTable,
			ParentField: table.ParentField,
		})
	}
	return result
}

func (s *Server) describeAPITable(principal Principal, tableName string) (apiTableSchema, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, false)
	if err != nil {
		return apiTableSchema{}, status, err
	}

	fields := make([]apiTableField, 0, len(table.Fields))
	for _, field := range table.Fields {
		schemaField := apiTableField{
			Column:      field.Column,
			Label:       field.Label,
			Kind:        string(field.Kind),
			Required:    field.Required,
			Editable:    field.Editable,
			List:        field.List,
			Sortable:    field.Sortable,
			Options:     field.Options,
			RefTable:    field.RefTable,
			Accept:      field.Accept,
			Placeholder: field.Placeholder,
		}
		fields = append(fields, schemaField)
	}

	return apiTableSchema{
		Name:          table.Name,
		Label:         table.Label,
		PrimaryKey:    table.PrimaryKey,
		TitleColumn:   table.TitleColumn,
		DefaultSort:   table.DefaultSort,
		DefaultDesc:   table.DefaultDesc,
		CanWrite:      table.CanWrite(principal.Role),
		ImportEnabled: table.ImportEnabled,
		IsSubtable:    table.IsSubtable(),
		ParentTable:   table.ParentTable,
		ParentField:   table.ParentField,
		ParentLabel:   table.ParentLabel,
		Fields:        fields,
	}, 0, nil
}

func (s *Server) apiListRecords(ctx context.Context, principal Principal, tableName string, input tableListInput) (apiTableListResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, false)
	if err != nil {
		return apiTableListResponse{}, status, err
	}

	filter, status, err := resolveParentFilter(table, input.ParentField, input.ParentID)
	if err != nil {
		return apiTableListResponse{}, status, err
	}
	equals, status, err := resolveColumnFilters(table, input.Equals)
	if err != nil {
		return apiTableListResponse{}, status, err
	}
	for column, value := range equals {
		if filter == nil {
			filter = map[string]any{}
		}
		filter[column] = value
	}
	from, status, err := resolveColumnFilters(table, input.From)
	if err != nil {
		return apiTableListResponse{}, status, err
	}
	to, status, err := resolveColumnFilters(table, input.To)
	if err != nil {
		return apiTableListResponse{}, status, err
	}

	result, err := s.store.List(ctx, table.Name, store.ListOptions{
		Sort:          table.SortColumn(input.Sort),
		Desc:          input.Desc,
		Limit:         input.Limit,
		Offset:        input.Offset,
		Filter:        filter,
		From:          from,
		To:            to,
		Search:        input.Search,
		SearchColumns: searchableColumns(table),
	})
	if err != nil {
		return apiTableListResponse{}, 500, err
	}

	return apiTableListResponse{
		Table:   table.Name,
		Rows:    result.Rows,
		HasMore: result.HasMore,
	}, 0, nil
}

// resolveParentFilter converts optional parent_field/parent_id inputs into a
// single-column filter usable by the store. Empty parent_id means no parent
// filter; a populated parent_id is only allowed on subtables and must target
// that subtable's declared parent field, so callers cannot use this to filter
// by arbitrary columns.
func resolveParentFilter(table store.TableDef, parentField, parentID string) (map[string]any, int, error) {
	parentID = strings.TrimSpace(parentID)
	parentField = strings.TrimSpace(parentField)
	if parentID == "" {
		if parentField != "" {
			return nil, 400, fmt.Errorf("parent_id is required when parent_field is set")
		}
		return nil, 0, nil
	}
	if !table.IsSubtable() {
		return nil, 400, fmt.Errorf("table %q does not support parent filtering", table.Name)
	}
	if parentField == "" {
		parentField = table.ParentField
	}
	if parentField != table.ParentField {
		return nil, 400, fmt.Errorf("parent_field must be %q for table %q", table.ParentField, table.Name)
	}
	field, ok := table.Field(parentField)
	if !ok {
		return nil, 500, fmt.Errorf("unknown parent field %q for table %q", parentField, table.Name)
	}
	parsed, err := store.ParseFieldValue(field, parentID)
	if err != nil || parsed == nil {
		return nil, 400, fmt.Errorf("invalid parent_id")
	}
	return map[string]any{field.Column: parsed}, 0, nil
}

// resolveColumnFilters parses raw query filters against the table schema so a
// caller can only filter on declared columns, with values coerced to the
// column's own type instead of being interpolated as text.
func resolveColumnFilters(table store.TableDef, raw map[string]string) (map[string]any, int, error) {
	if len(raw) == 0 {
		return nil, 0, nil
	}
	resolved := make(map[string]any, len(raw))
	for column, value := range raw {
		field, ok := table.Field(column)
		if !ok {
			return nil, 400, fmt.Errorf("unknown filter column %q", column)
		}
		parsed, err := store.ParseFieldValue(field, value)
		if err != nil || parsed == nil {
			return nil, 400, fmt.Errorf("invalid value for filter column %q", column)
		}
		resolved[field.Column] = parsed
	}
	return resolved, 0, nil
}

// searchableColumns lists the free-text columns a search query may match.
func searchableColumns(table store.TableDef) []string {
	columns := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		if field.Kind == store.KindText || field.Kind == store.KindTextarea {
			columns = append(columns, field.Column)
		}
	}
	return columns
}

func (s *Server) apiGetRecord(ctx context.Context, principal Principal, tableName, id string) (apiTableRowResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, false)
	if err != nil {
		return apiTableRowResponse{}, status, err
	}

	normalizedID, status, err := normalizeRecordID(table, id)
	if err != nil {
		return apiTableRowResponse{}, status, err
	}

	row, err := s.store.Get(ctx, table.Name, normalizedID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apiTableRowResponse{}, 404, fmt.Errorf("row not found")
		}
		return apiTableRowResponse{}, 500, err
	}

	return apiTableRowResponse{
		Table: table.Name,
		ID:    normalizedID,
		Row:   row,
	}, 0, nil
}

func (s *Server) apiCreateRecord(ctx context.Context, principal Principal, tableName string, payload map[string]any) (apiTableRowResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, true)
	if err != nil {
		return apiTableRowResponse{}, status, err
	}

	values, err := s.parseAPIValues(table, payload, true)
	if err != nil {
		return apiTableRowResponse{}, 400, err
	}
	s.applyAutomaticUserID(table, principal, true, values)

	if err := validateBusinessRules(table, values); err != nil {
		return apiTableRowResponse{}, 400, err
	}

	id, err := s.store.Insert(ctx, table.Name, values)
	if err != nil {
		return apiTableRowResponse{}, classifyStoreError(err), err
	}

	insertedID := strconv.FormatInt(id, 10)
	row, err := s.store.Get(ctx, table.Name, insertedID)
	if err != nil {
		return apiTableRowResponse{}, 500, err
	}

	return apiTableRowResponse{
		Table: table.Name,
		ID:    insertedID,
		Row:   row,
	}, 201, nil
}

func (s *Server) apiUpdateRecord(ctx context.Context, principal Principal, tableName, id string, payload map[string]any) (apiTableRowResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, true)
	if err != nil {
		return apiTableRowResponse{}, status, err
	}

	normalizedID, status, err := normalizeRecordID(table, id)
	if err != nil {
		return apiTableRowResponse{}, status, err
	}

	values, err := s.parseAPIValues(table, payload, false)
	if err != nil {
		return apiTableRowResponse{}, 400, err
	}
	s.applyAutomaticUserID(table, principal, false, values)

	if err := validateBusinessRules(table, values); err != nil {
		return apiTableRowResponse{}, 400, err
	}

	if err := s.store.Update(ctx, table.Name, normalizedID, values); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apiTableRowResponse{}, 404, fmt.Errorf("row not found")
		}
		return apiTableRowResponse{}, classifyStoreError(err), err
	}

	row, err := s.store.Get(ctx, table.Name, normalizedID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apiTableRowResponse{}, 404, fmt.Errorf("row not found")
		}
		return apiTableRowResponse{}, 500, err
	}

	return apiTableRowResponse{
		Table: table.Name,
		ID:    normalizedID,
		Row:   row,
	}, 200, nil
}

func (s *Server) apiDeleteRecord(ctx context.Context, principal Principal, tableName, id string) (apiTableDeleteResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, true)
	if err != nil {
		return apiTableDeleteResponse{}, status, err
	}

	normalizedID, status, err := normalizeRecordID(table, id)
	if err != nil {
		return apiTableDeleteResponse{}, status, err
	}

	if table.Name == "users" {
		record, err := s.store.Get(ctx, table.Name, normalizedID)
		if err == nil && fmt.Sprint(record["usr_role"]) == "admin" {
			admins, err := s.store.CountAdmins(ctx)
			if err != nil {
				return apiTableDeleteResponse{}, 500, err
			}
			if admins <= 1 {
				return apiTableDeleteResponse{}, 409, fmt.Errorf("deleting the last admin user is blocked")
			}
		}
	}

	if err := s.store.Delete(ctx, table.Name, normalizedID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return apiTableDeleteResponse{}, 404, fmt.Errorf("row not found")
		}
		return apiTableDeleteResponse{}, classifyStoreError(err), err
	}

	return apiTableDeleteResponse{
		Table:   table.Name,
		ID:      normalizedID,
		Deleted: true,
	}, 0, nil
}

func (s *Server) apiImportCSV(ctx context.Context, principal Principal, tableName string, reader io.Reader) (apiTableImportResponse, int, error) {
	table, status, err := s.resolveTableForRole(principal.Role, tableName, true)
	if err != nil {
		return apiTableImportResponse{}, status, err
	}
	if !table.ImportEnabled {
		return apiTableImportResponse{}, 400, fmt.Errorf("CSV import is not enabled for table %q", table.Name)
	}
	if reader == nil {
		return apiTableImportResponse{}, 400, fmt.Errorf("csv payload is required")
	}

	imported, err := s.store.ImportCSV(ctx, table.Name, reader, store.ImportOptions{
		Transform:    csvImportTransform,
		BeforeInsert: func(values map[string]any) error { return validateBusinessRules(table, values) },
	})
	if err != nil {
		status := classifyStoreError(err)
		if status == 500 {
			status = 400
		}
		return apiTableImportResponse{}, status, err
	}
	return apiTableImportResponse{Table: table.Name, Imported: imported}, 200, nil
}

// csvImportTransform mirrors the behavior of the HTML form import: text is
// passed through ParseFieldValue, and password cells are Argon2id-hashed
// (empty passwords are left unset so existing hashes survive re-import).
func csvImportTransform(field store.Field, raw string) (any, error) {
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
}

func (s *Server) resolveTableForRole(role, tableName string, write bool) (store.TableDef, int, error) {
	tableName = strings.TrimSpace(tableName)
	table, ok := s.store.Table(tableName)
	if !ok {
		return store.TableDef{}, 404, fmt.Errorf("table unavailable")
	}
	if write && !table.CanWrite(role) {
		return store.TableDef{}, 403, fmt.Errorf("forbidden")
	}
	if !write && !table.CanRead(role) {
		return store.TableDef{}, 403, fmt.Errorf("forbidden")
	}
	return table, 0, nil
}

func normalizeRecordID(table store.TableDef, rawID string) (string, int, error) {
	rawID = strings.TrimSpace(rawID)
	if rawID == "" {
		return "", 400, fmt.Errorf("id is required")
	}

	field, ok := table.Field(table.PrimaryKey)
	if !ok {
		return "", 500, fmt.Errorf("primary key %q is not defined", table.PrimaryKey)
	}

	value, err := store.ParseFieldValue(field, rawID)
	if err != nil || value == nil {
		return "", 400, fmt.Errorf("invalid id")
	}

	return fmt.Sprint(value), 0, nil
}

func parseListInput(limitValue, offsetValue, sortValue, descValue, parentFieldValue, parentIDValue string) (tableListInput, error) {
	limit, err := parseBoundedInt(limitValue, 30, 1, 200, "limit")
	if err != nil {
		return tableListInput{}, err
	}
	offset, err := parseBoundedInt(offsetValue, 0, 0, 1_000_000, "offset")
	if err != nil {
		return tableListInput{}, err
	}
	desc, err := parseStrictBool(descValue, false)
	if err != nil {
		return tableListInput{}, fmt.Errorf("invalid desc: %w", err)
	}

	return tableListInput{
		Sort:        strings.TrimSpace(sortValue),
		Desc:        desc,
		Limit:       limit,
		Offset:      offset,
		ParentField: strings.TrimSpace(parentFieldValue),
		ParentID:    strings.TrimSpace(parentIDValue),
	}, nil
}

// prefixedQueryFilters collects "<prefix>.<column>=<value>" query parameters,
// which is how REST callers express equality and range filters.
func prefixedQueryFilters(query url.Values, prefix string) map[string]string {
	filters := map[string]string{}
	for key, values := range query {
		column, ok := strings.CutPrefix(key, prefix+".")
		if !ok || len(values) == 0 {
			continue
		}
		column = strings.TrimSpace(column)
		value := strings.TrimSpace(values[0])
		if column == "" || value == "" {
			continue
		}
		filters[column] = value
	}
	if len(filters) == 0 {
		return nil
	}
	return filters
}

func parseBoundedInt(raw string, defaultValue, minValue, maxValue int, fieldName string) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}

	parsed, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", fieldName)
	}
	if parsed < minValue || parsed > maxValue {
		return 0, fmt.Errorf("%s must be between %d and %d", fieldName, minValue, maxValue)
	}
	return parsed, nil
}

// validateBusinessRules applies cross-field constraints that belong to the
// application layer (not enforceable by SQLite alone) and must hold regardless
// of whether the request came from REST, MCP, the HTML form, or CSV import.
func validateBusinessRules(table store.TableDef, values map[string]any) error {
	if table.Name == "stock_moves" {
		src, okSrc := values["stm_src_loc_id"]
		dst, okDst := values["stm_dst_loc_id"]
		if okSrc && src != nil && okDst && dst != nil && fmt.Sprint(src) == fmt.Sprint(dst) {
			return fmt.Errorf("Source and destination locations must be different")
		}
	}
	if table.Name == "financial_obligations" {
		if amount, ok := values["fob_amount_minor"].(int64); ok && amount <= 0 {
			return fmt.Errorf("fob_amount_minor must be positive")
		}
		if amount, ok := values["fob_amount_minor"].(float64); ok && amount <= 0 {
			return fmt.Errorf("fob_amount_minor must be positive")
		}
	}
	if table.Name == "bank_transactions" {
		if amount, ok := values["btx_amount_minor"].(int64); ok && amount == 0 {
			return fmt.Errorf("btx_amount_minor must not be zero")
		}
	}
	return nil
}

// classifyStoreError maps a store/sql error to an HTTP status code. Constraint
// failures are caller-fixable (409/400); anything else is treated as a server
// error so transient DB problems do not surface as 400 to clients.
func classifyStoreError(err error) int {
	if err == nil {
		return 0
	}
	// A workflow rule the store enforces (a status only the approve endpoint may
	// set) is the caller's mistake, not a server fault.
	if status := workflowStatus(err); status != 500 {
		return status
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return 409
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return 409
	case strings.Contains(msg, "CHECK constraint failed"),
		strings.Contains(msg, "NOT NULL constraint failed"):
		return 400
	default:
		return 500
	}
}

func parseStrictBool(raw string, defaultValue bool) (bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}

	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("must be a boolean")
	}
}
