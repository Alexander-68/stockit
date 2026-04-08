package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"

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

type apiErrorResponse struct {
	Error string `json:"error"`
}

type apiMeResponse struct {
	User string `json:"user"`
	Role string `json:"role"`
}

type apiTableListEnvelope struct {
	Tables []apiTableSummary `json:"tables"`
}

type apiTableSchemaEnvelope struct {
	Table apiTableSchema `json:"table"`
}

type tableListInput struct {
	Sort   string
	Desc   bool
	Limit  int
	Offset int
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

	result, err := s.store.List(ctx, table.Name, store.ListOptions{
		Sort:   table.SortColumn(input.Sort),
		Desc:   input.Desc,
		Limit:  input.Limit,
		Offset: input.Offset,
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

func parseListInput(limitValue, offsetValue, sortValue, descValue string) (tableListInput, error) {
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
		Sort:   strings.TrimSpace(sortValue),
		Desc:   desc,
		Limit:  limit,
		Offset: offset,
	}, nil
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

// classifyStoreError maps a store/sql error to an HTTP status code. Constraint
// failures are caller-fixable (409/400); anything else is treated as a server
// error so transient DB problems do not surface as 400 to clients.
func classifyStoreError(err error) int {
	if err == nil {
		return 0
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
