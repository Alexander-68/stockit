package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const (
	mcpToolListTables   = "stockit_list_tables"
	mcpToolDescribe     = "stockit_describe_table"
	mcpToolListRecords  = "stockit_list_records"
	mcpToolGetRecord    = "stockit_get_record"
	mcpToolCreateRecord = "stockit_create_record"
	mcpToolUpdateRecord = "stockit_update_record"
	mcpToolDeleteRecord = "stockit_delete_record"
	mcpToolImportCSV    = "stockit_import_csv"
)

type mcpDescribeTableArgs struct {
	Table string `json:"table"`
}

type mcpListRecordsArgs struct {
	Table       string `json:"table"`
	Sort        string `json:"sort"`
	Desc        bool   `json:"desc"`
	Limit       int    `json:"limit"`
	Offset      int    `json:"offset"`
	ParentField string `json:"parent_field"`
	ParentID    string `json:"parent_id"`
}

type mcpGetRecordArgs struct {
	Table string `json:"table"`
	ID    string `json:"id"`
}

type mcpCreateRecordArgs struct {
	Table  string         `json:"table"`
	Values map[string]any `json:"values"`
}

type mcpUpdateRecordArgs struct {
	Table  string         `json:"table"`
	ID     string         `json:"id"`
	Values map[string]any `json:"values"`
}

type mcpDeleteRecordArgs struct {
	Table string `json:"table"`
	ID    string `json:"id"`
}

type mcpImportCSVArgs struct {
	Table string `json:"table"`
	CSV   string `json:"csv"`
}

func (s *Server) newMCPHandler() httpHandlerWithErr {
	mcpSrv := mcpserver.NewMCPServer(
		"StockIt",
		"1.0.0",
		mcpserver.WithToolCapabilities(true),
		mcpserver.WithRecovery(),
		mcpserver.WithInstructions("Use StockIt tools for warehouse data access. Authenticate first, then prefer the listed tools over direct database assumptions. API and MCP operations are intentionally aligned."),
		mcpserver.WithToolFilter(func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
			principal := principalFromContext(ctx)
			if s.roleHasAnyWritableTable(principal.Role) {
				return tools
			}

			filtered := make([]mcp.Tool, 0, len(tools))
			for _, tool := range tools {
				switch tool.Name {
				case mcpToolCreateRecord, mcpToolUpdateRecord, mcpToolDeleteRecord, mcpToolImportCSV:
					continue
				default:
					filtered = append(filtered, tool)
				}
			}
			return filtered
		}),
	)

	s.registerMCPTools(mcpSrv)

	streamable := mcpserver.NewStreamableHTTPServer(
		mcpSrv,
		mcpserver.WithStateful(true),
		mcpserver.WithSessionIdleTTL(sessionIdleTimeout),
	)

	return func(w http.ResponseWriter, r *http.Request) error {
		principal, ok := s.principalFromRequest(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return nil
		}

		ctx := context.WithValue(r.Context(), principalKey, principal)
		streamable.ServeHTTP(w, r.WithContext(ctx))
		return nil
	}
}

type httpHandlerWithErr func(http.ResponseWriter, *http.Request) error

func (h httpHandlerWithErr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = h(w, r)
}

func (s *Server) registerMCPTools(mcpSrv *mcpserver.MCPServer) {
	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolListTables,
			mcp.WithDescription("List the StockIt tables available to the authenticated user."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
		),
		s.handleMCPListTables,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolDescribe,
			mcp.WithDescription("Describe a StockIt table schema, permissions, and field metadata."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
		),
		s.handleMCPDescribeTable,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolListRecords,
			mcp.WithDescription("List records from a StockIt table using the same rules as the REST API."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithString("sort", mcp.Description("Sortable column name")),
			mcp.WithBoolean("desc", mcp.Description("Whether to sort descending")),
			mcp.WithNumber("limit", mcp.Description("Page size from 1 to 200; omit or pass 0 for the default of 30")),
			mcp.WithNumber("offset", mcp.Description("Row offset starting at 0")),
			mcp.WithString("parent_id", mcp.Description("Restrict to rows whose parent foreign key equals this id; only valid for subtables")),
			mcp.WithString("parent_field", mcp.Description("Optional subtable parent column name; defaults to the table's declared parent field")),
		),
		s.handleMCPListRecords,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolGetRecord,
			mcp.WithDescription("Fetch one StockIt record by table and id."),
			mcp.WithReadOnlyHintAnnotation(true),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithString("id", mcp.Required(), mcp.Description("Primary key value")),
		),
		s.handleMCPGetRecord,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolCreateRecord,
			mcp.WithDescription("Create one StockIt record through the validated application API layer."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithObject("values", mcp.Required(), mcp.Description("Field values keyed by column name"), mcp.AdditionalProperties(true)),
		),
		s.handleMCPCreateRecord,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolUpdateRecord,
			mcp.WithDescription("Update one StockIt record through the validated application API layer."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithString("id", mcp.Required(), mcp.Description("Primary key value")),
			mcp.WithObject("values", mcp.Required(), mcp.Description("Field values keyed by column name"), mcp.AdditionalProperties(true)),
		),
		s.handleMCPUpdateRecord,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolDeleteRecord,
			mcp.WithDescription("Delete one StockIt record through the validated application API layer."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithString("id", mcp.Required(), mcp.Description("Primary key value")),
		),
		s.handleMCPDeleteRecord,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolImportCSV,
			mcp.WithDescription("Bulk-import rows into a StockIt table from a CSV payload using the same rules as the REST API and web UI."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(true),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("table", mcp.Required(), mcp.Description("StockIt table name")),
			mcp.WithString("csv", mcp.Required(), mcp.Description("CSV document including a header row. Column headers match table field columns or labels.")),
		),
		s.handleMCPImportCSV,
	)
}

func (s *Server) handleMCPListTables(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	principal := principalFromContext(ctx)
	result := apiTableListEnvelope{Tables: s.listAPITables(principal)}
	return mcp.NewToolResultStructured(result, fmt.Sprintf("Listed %d StockIt tables.", len(result.Tables))), nil
}

func (s *Server) handleMCPDescribeTable(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpDescribeTableArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	table, _, err := s.describeAPITable(principal, args.Table)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(apiTableSchemaEnvelope{Table: table}, "Described StockIt table."), nil
}

func (s *Server) handleMCPListRecords(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpListRecordsArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	input, err := mcpListInput(args)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiListRecords(ctx, principal, args.Table, input)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Listed %d records from %s.", len(result.Rows), result.Table)), nil
}

func (s *Server) handleMCPGetRecord(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpGetRecordArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiGetRecord(ctx, principal, args.Table, args.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Fetched %s record %s.", result.Table, result.ID)), nil
}

func (s *Server) handleMCPCreateRecord(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpCreateRecordArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if args.Values == nil {
		return mcp.NewToolResultError("values is required"), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiCreateRecord(ctx, principal, args.Table, args.Values)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Created record in %s.", result.Table)), nil
}

func (s *Server) handleMCPUpdateRecord(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpUpdateRecordArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if args.Values == nil {
		return mcp.NewToolResultError("values is required"), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiUpdateRecord(ctx, principal, args.Table, args.ID, args.Values)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Updated %s record %s.", result.Table, result.ID)), nil
}

func (s *Server) handleMCPDeleteRecord(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpDeleteRecordArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiDeleteRecord(ctx, principal, args.Table, args.ID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Deleted %s record %s.", result.Table, result.ID)), nil
}

func (s *Server) handleMCPImportCSV(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpImportCSVArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	if strings.TrimSpace(args.CSV) == "" {
		return mcp.NewToolResultError("csv is required"), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiImportCSV(ctx, principal, args.Table, strings.NewReader(args.CSV))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Imported %d rows into %s.", result.Imported, result.Table)), nil
}

func mcpListInput(args mcpListRecordsArgs) (tableListInput, error) {
	limit := args.Limit
	if limit == 0 {
		limit = 30
	}
	if limit < 1 || limit > 200 {
		return tableListInput{}, fmt.Errorf("limit must be between 1 and 200")
	}
	if args.Offset < 0 {
		return tableListInput{}, fmt.Errorf("offset must be 0 or greater")
	}

	return tableListInput{
		Sort:        args.Sort,
		Desc:        args.Desc,
		Limit:       limit,
		Offset:      args.Offset,
		ParentField: args.ParentField,
		ParentID:    args.ParentID,
	}, nil
}
