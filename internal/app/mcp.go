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

	mcpToolSubmitRequisition = "stockit_submit_requisition"
	mcpToolDecideApproval    = "stockit_decide_approval"
	mcpToolRequisitionToPO   = "stockit_create_po_from_requisition"
)

type mcpSubmitRequisitionArgs struct {
	RequisitionID string `json:"requisition_id"`
}

type mcpDecideApprovalArgs struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
	Note       string `json:"note"`
}

type mcpRequisitionToPOArgs struct {
	RequisitionID string `json:"requisition_id"`
	DocNumber     string `json:"por_doc_number"`
	SupplierID    *int64 `json:"sup_id"`
}

type mcpDescribeTableArgs struct {
	Table string `json:"table"`
}

type mcpListRecordsArgs struct {
	Table       string            `json:"table"`
	Sort        string            `json:"sort"`
	Desc        bool              `json:"desc"`
	Limit       int               `json:"limit"`
	Offset      int               `json:"offset"`
	ParentField string            `json:"parent_field"`
	ParentID    string            `json:"parent_id"`
	Filter      map[string]string `json:"filter"`
	From        map[string]string `json:"from"`
	To          map[string]string `json:"to"`
	Search      string            `json:"search"`
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

func (s *Server) newMCPHandler() http.Handler {
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
				case mcpToolCreateRecord, mcpToolUpdateRecord, mcpToolDeleteRecord, mcpToolImportCSV,
					mcpToolSubmitRequisition, mcpToolDecideApproval, mcpToolRequisitionToPO:
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

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := s.principalFromRequest(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), principalKey, principal)
		streamable.ServeHTTP(w, r.WithContext(ctx))
	})
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
			mcp.WithObject("filter", mcp.Description("Exact-match filters keyed by column name, for example {\"por_status\":\"draft\"}"), mcp.AdditionalProperties(true)),
			mcp.WithObject("from", mcp.Description("Inclusive lower bounds keyed by column name, for example {\"por_doc_date\":\"2026-01-01\"}"), mcp.AdditionalProperties(true)),
			mcp.WithObject("to", mcp.Description("Inclusive upper bounds keyed by column name, for example {\"por_doc_date\":\"2026-12-31\"}"), mcp.AdditionalProperties(true)),
			mcp.WithString("search", mcp.Description("Case-insensitive substring matched against the table's text columns")),
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

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolSubmitRequisition,
			mcp.WithDescription("Submit a draft purchase requisition for approval; prices it from its lines and creates the approval steps its amount requires."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("requisition_id", mcp.Required(), mcp.Description("purchase_requisitions.prq_id")),
		),
		s.handleMCPSubmitRequisition,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolDecideApproval,
			mcp.WithDescription("Approve or reject one pending approval step. Only a user holding the step's role may decide it, and steps are decided in order."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("approval_id", mcp.Required(), mcp.Description("approvals.apv_id")),
			mcp.WithString("decision", mcp.Required(), mcp.Description("approved or rejected")),
			mcp.WithString("note", mcp.Description("Optional decision note kept in the audit trail")),
		),
		s.handleMCPDecideApproval,
	)

	mcpSrv.AddTool(
		mcp.NewTool(
			mcpToolRequisitionToPO,
			mcp.WithDescription("Create a draft purchase order from an approved requisition, copying its lines."),
			mcp.WithReadOnlyHintAnnotation(false),
			mcp.WithDestructiveHintAnnotation(false),
			mcp.WithSchemaAdditionalProperties(false),
			mcp.WithString("requisition_id", mcp.Required(), mcp.Description("purchase_requisitions.prq_id")),
			mcp.WithString("por_doc_number", mcp.Description("Purchase order document number; defaults to the requisition document number")),
			mcp.WithNumber("sup_id", mcp.Description("Supplier id; defaults to the requisition's suggested supplier")),
		),
		s.handleMCPRequisitionToPO,
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

func (s *Server) handleMCPSubmitRequisition(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpSubmitRequisitionArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiSubmitRequisition(ctx, principal, args.RequisitionID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Requisition %d is now %s with %d approval steps.", result.RequisitionID, result.Status, len(result.Approvals))), nil
}

func (s *Server) handleMCPDecideApproval(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpDecideApprovalArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiDecideApproval(ctx, principal, args.ApprovalID, args.Decision, args.Note)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Requisition %d is now %s with %d approvals still pending.", result.RequisitionID, result.RequisitionStatus, result.RemainingApprovals)), nil
}

func (s *Server) handleMCPRequisitionToPO(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args mcpRequisitionToPOArgs
	if err := request.BindArguments(&args); err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	principal := principalFromContext(ctx)
	result, _, err := s.apiCreatePOFromRequisition(ctx, principal, args.RequisitionID, args.DocNumber, args.SupplierID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	return mcp.NewToolResultStructured(result, fmt.Sprintf("Created a purchase order from requisition %d.", result.RequisitionID)), nil
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
		Equals:      args.Filter,
		From:        args.From,
		To:          args.To,
		Search:      strings.TrimSpace(args.Search),
	}, nil
}
