package store

import (
	"context"
	"strconv"
	"testing"
)

// seedPurchaseOrder creates one supplier, one item and a draft purchase order
// with a single line, and returns the purchase order id as a string.
func seedPurchaseOrder(t *testing.T, s *Store, ctx context.Context, qty, price float64) string {
	t.Helper()

	supplier, err := s.Insert(ctx, "suppliers", map[string]any{"sup_name_en": "PO Status Supplier", "sup_status": "Active"})
	if err != nil {
		t.Fatalf("insert supplier: %v", err)
	}
	item, err := s.Insert(ctx, "items", map[string]any{"itm_sku": "PO-STAT-1", "itm_status": "Active"})
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}
	purchaseOrder, err := s.Insert(ctx, "purchase_orders", map[string]any{
		"por_doc_number": "PO-STAT-1", "por_doc_date": "2026-08-01", "sup_id": supplier,
		"por_status": "draft", "por_payment_status": "unpaid", "por_currency": "USD",
	})
	if err != nil {
		t.Fatalf("insert purchase order: %v", err)
	}
	if _, err := s.Insert(ctx, "po_components", map[string]any{
		"por_id": purchaseOrder, "itm_id": item, "poc_qty": qty, "poc_price": price, "poc_currency": "USD",
	}); err != nil {
		t.Fatalf("insert po line: %v", err)
	}
	return strconv.FormatInt(purchaseOrder, 10)
}

func poHistory(t *testing.T, s *Store, ctx context.Context, purchaseOrderID string) []map[string]any {
	t.Helper()

	result, err := s.List(ctx, "po_status_history", ListOptions{
		Sort: "psh_id", Limit: 50, Filter: map[string]any{"por_id": purchaseOrderID},
	})
	if err != nil {
		t.Fatalf("list status history: %v", err)
	}
	return result.Rows
}

func TestPurchaseOrderStatusHistoryRecordsEveryChange(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := WithActor(context.Background(), 1)
	purchaseOrder := seedPurchaseOrder(t, s, ctx, 10, 5)

	// The creating insert is itself a status change and must be on record.
	if rows := poHistory(t, s, ctx, purchaseOrder); len(rows) != 1 || rows[0]["psh_status"] != "draft" {
		t.Fatalf("history after create = %+v", rows)
	}

	// A plain table update still lands in the trail, with no note.
	if err := s.Update(ctx, "purchase_orders", purchaseOrder, map[string]any{"por_status": "issued"}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	// A field that is not a status must not add a row.
	if err := s.Update(ctx, "purchase_orders", purchaseOrder, map[string]any{"por_note": "unrelated"}); err != nil {
		t.Fatalf("update note: %v", err)
	}
	rows := poHistory(t, s, ctx, purchaseOrder)
	if len(rows) != 2 {
		t.Fatalf("history rows = %d, want 2: %+v", len(rows), rows)
	}
	if rows[1]["psh_previous_status"] != "draft" || rows[1]["psh_status"] != "issued" {
		t.Fatalf("unexpected transition: %+v", rows[1])
	}

	// SetPOStatus carries the note and the payment tag.
	row, err := s.SetPOStatus(ctx, mustParseInt(t, purchaseOrder), "closed", "paid", "supplier paid in full")
	if err != nil {
		t.Fatalf("set status: %v", err)
	}
	if row["por_status"] != "closed" || row["por_payment_status"] != "paid" {
		t.Fatalf("unexpected purchase order after status change: %+v", row)
	}
	rows = poHistory(t, s, ctx, purchaseOrder)
	last := rows[len(rows)-1]
	if last["psh_status"] != "closed" || last["psh_payment_status"] != "paid" || last["psh_note"] != "supplier paid in full" {
		t.Fatalf("unexpected history entry: %+v", last)
	}
	if last["usr_id"] == nil {
		t.Fatalf("history entry lost the acting user: %+v", last)
	}
}

func TestSetPOStatusRejectsUnknownValues(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := WithActor(context.Background(), 1)
	purchaseOrder := mustParseInt(t, seedPurchaseOrder(t, s, ctx, 1, 1))

	if _, err := s.SetPOStatus(ctx, purchaseOrder, "shipped", "", ""); err == nil {
		t.Fatal("expected a legacy status to be rejected")
	}
	if _, err := s.SetPOStatus(ctx, purchaseOrder, "", "overpaid", ""); err == nil {
		t.Fatal("expected an unknown payment tag to be rejected")
	}
	if _, err := s.SetPOStatus(ctx, purchaseOrder, "", "", "note only"); err == nil {
		t.Fatal("expected a status change with nothing to change to be rejected")
	}
}

func TestReceiptQuantitiesDerivePurchaseOrderStatus(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := WithActor(context.Background(), 1)
	purchaseOrder := seedPurchaseOrder(t, s, ctx, 10, 5)
	lineID := onlyLineID(t, s, ctx, purchaseOrder)

	// A draft order is never moved by a receipt.
	if err := s.Update(ctx, "po_components", lineID, map[string]any{"poc_received_qty": 4}); err != nil {
		t.Fatalf("receive partial on draft: %v", err)
	}
	if status := poStatus(t, s, ctx, purchaseOrder); status != "draft" {
		t.Fatalf("draft order status = %q, want draft", status)
	}

	if _, err := s.SetPOStatus(ctx, mustParseInt(t, purchaseOrder), "issued", "", "sent to supplier"); err != nil {
		t.Fatalf("issue order: %v", err)
	}
	if err := s.Update(ctx, "po_components", lineID, map[string]any{"poc_received_qty": 4}); err != nil {
		t.Fatalf("receive partial: %v", err)
	}
	if status := poStatus(t, s, ctx, purchaseOrder); status != "partially_received" {
		t.Fatalf("status after partial receipt = %q", status)
	}

	if err := s.Update(ctx, "po_components", lineID, map[string]any{"poc_received_qty": 10}); err != nil {
		t.Fatalf("receive all: %v", err)
	}
	if status := poStatus(t, s, ctx, purchaseOrder); status != "received" {
		t.Fatalf("status after full receipt = %q", status)
	}

	rows := poHistory(t, s, ctx, purchaseOrder)
	last := rows[len(rows)-1]
	if last["psh_status"] != "received" || last["psh_note"] != "derived from line receipts" {
		t.Fatalf("derived change not recorded: %+v", last)
	}

	// A closed order stays closed even if a receipt is corrected afterwards.
	if _, err := s.SetPOStatus(ctx, mustParseInt(t, purchaseOrder), "closed", "paid", "done"); err != nil {
		t.Fatalf("close order: %v", err)
	}
	if err := s.Update(ctx, "po_components", lineID, map[string]any{"poc_received_qty": 5}); err != nil {
		t.Fatalf("correct receipt: %v", err)
	}
	if status := poStatus(t, s, ctx, purchaseOrder); status != "closed" {
		t.Fatalf("closed order was moved to %q", status)
	}
}

func TestPurchaseOrderApprovalWorkflow(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := WithActor(context.Background(), 1)
	if _, err := s.Insert(ctx, "approval_rules", map[string]any{
		"apr_source_type": "purchase_order", "apr_step": 1, "apr_role": "admin",
		"apr_min_amount_minor": 10_000, "apr_status": "active",
	}); err != nil {
		t.Fatalf("insert approval rule: %v", err)
	}

	purchaseOrder := seedPurchaseOrder(t, s, ctx, 10, 50)
	submission, err := s.SubmitPurchaseOrder(ctx, mustParseInt(t, purchaseOrder))
	if err != nil {
		t.Fatalf("submit purchase order: %v", err)
	}
	if submission.Status != "pending_approval" || submission.TotalMinor != 50_000 || len(submission.Approvals) != 1 {
		t.Fatalf("unexpected submission: %+v", submission)
	}
	if _, err := s.SubmitPurchaseOrder(ctx, mustParseInt(t, purchaseOrder)); err == nil {
		t.Fatal("expected a second submit of a routing order to be refused")
	}

	approvalID := int64(submission.Approvals[0]["apv_id"].(int64))
	if _, err := s.DecideApproval(ctx, approvalID, "user", 2, true, ""); err == nil {
		t.Fatal("expected the wrong role to be refused")
	}
	decision, err := s.DecideApproval(ctx, approvalID, "admin", 1, true, "budget ok")
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if decision.Status != "approved" || decision.SourceType != "purchase_order" {
		t.Fatalf("unexpected decision: %+v", decision)
	}
	if status := poStatus(t, s, ctx, purchaseOrder); status != "approved" {
		t.Fatalf("purchase order status = %q, want approved", status)
	}

	rows := poHistory(t, s, ctx, purchaseOrder)
	last := rows[len(rows)-1]
	if last["psh_status"] != "approved" || last["psh_note"] != "approval step 1 approved: budget ok" {
		t.Fatalf("approval not recorded in history: %+v", last)
	}
}

func TestLegacyPurchaseOrderStatusesMigrate(t *testing.T) {
	dbPath := t.TempDir() + "/stockit.db"
	ctx := context.Background()

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// Write the pre-lifecycle values straight past the enum, as an old build would have.
	for _, legacy := range []string{"sent", "prepared", "shipped", "delivered", "paid", "complete", "inactive"} {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO purchase_orders (por_doc_number, por_status) VALUES (?, ?)`, "LEGACY-"+legacy, legacy); err != nil {
			t.Fatalf("seed legacy status %q: %v", legacy, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	want := map[string][2]string{
		"LEGACY-sent":      {"issued", "unpaid"},
		"LEGACY-prepared":  {"confirmed", "unpaid"},
		"LEGACY-shipped":   {"confirmed", "unpaid"},
		"LEGACY-delivered": {"received", "unpaid"},
		"LEGACY-paid":      {"closed", "paid"},
		"LEGACY-complete":  {"closed", "unpaid"},
		"LEGACY-inactive":  {"cancelled", "unpaid"},
	}
	rows, err := s.List(ctx, "purchase_orders", ListOptions{Sort: "por_id", Limit: 50})
	if err != nil {
		t.Fatalf("list purchase orders: %v", err)
	}
	for _, row := range rows.Rows {
		expected, ok := want[row["por_doc_number"].(string)]
		if !ok {
			continue
		}
		if row["por_status"] != expected[0] || row["por_payment_status"] != expected[1] {
			t.Fatalf("%v migrated to %v/%v, want %v", row["por_doc_number"], row["por_status"], row["por_payment_status"], expected)
		}
		delete(want, row["por_doc_number"].(string))
	}
	if len(want) != 0 {
		t.Fatalf("legacy rows not seen: %+v", want)
	}
}

func poStatus(t *testing.T, s *Store, ctx context.Context, purchaseOrderID string) string {
	t.Helper()

	row, err := s.Get(ctx, "purchase_orders", purchaseOrderID)
	if err != nil {
		t.Fatalf("get purchase order: %v", err)
	}
	status, _ := row["por_status"].(string)
	return status
}

func onlyLineID(t *testing.T, s *Store, ctx context.Context, purchaseOrderID string) string {
	t.Helper()

	result, err := s.List(ctx, "po_components", ListOptions{Sort: "poc_id", Limit: 10, Filter: map[string]any{"por_id": purchaseOrderID}})
	if err != nil {
		t.Fatalf("list po lines: %v", err)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("expected one line, got %d", len(result.Rows))
	}
	return strconv.FormatInt(result.Rows[0]["poc_id"].(int64), 10)
}

func mustParseInt(t *testing.T, value string) int64 {
	t.Helper()

	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("parse id %q: %v", value, err)
	}
	return parsed
}

// TestLegacyRequisitionTablesAreDropped opens a database shaped like the one a
// pre-removal build wrote — requisition tables, a purchase_orders.prq_id link,
// and requisition approval rows — and checks the migration clears them without
// losing purchase orders or their lines.
func TestLegacyRequisitionTablesAreDropped(t *testing.T) {
	dbPath := t.TempDir() + "/stockit.db"
	ctx := context.Background()

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	for _, statement := range []string{
		`ALTER TABLE purchase_orders ADD COLUMN prq_id INTEGER REFERENCES purchase_requisitions (prq_id)`,
		`CREATE TABLE purchase_requisitions (prq_id INTEGER PRIMARY KEY AUTOINCREMENT, prq_doc_number TEXT, por_id INTEGER)`,
		`CREATE TABLE prq_components (prc_id INTEGER PRIMARY KEY AUTOINCREMENT, prq_id INTEGER NOT NULL, prc_qty REAL)`,
		`INSERT INTO purchase_requisitions (prq_id, prq_doc_number) VALUES (7, 'OLD-PRQ')`,
		`INSERT INTO prq_components (prq_id, prc_qty) VALUES (7, 3)`,
		`INSERT INTO items (itm_id, itm_sku, itm_status) VALUES (1, 'OLD-SKU', 'Active')`,
		`INSERT INTO purchase_orders (por_id, por_doc_number, por_status, prq_id) VALUES (1, 'OLD-PO', 'issued', 7)`,
		`INSERT INTO po_components (por_id, itm_id, poc_qty) VALUES (1, 1, 4)`,
		`INSERT INTO approval_rules (apr_source_type, apr_step, apr_role, apr_min_amount_minor, apr_status)
			VALUES ('purchase_requisition', 1, 'admin', 100, 'active')`,
		`INSERT INTO approvals (apv_source_type, apv_source_id, apv_step, apv_role, apv_status)
			VALUES ('purchase_requisition', 7, 1, 'admin', 'pending')`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed legacy schema %q: %v", statement, err)
		}
	}
	_ = s.Close()

	s, err = Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, table := range []string{"purchase_requisitions", "prq_components"} {
		var name string
		err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&name)
		if err == nil {
			t.Fatalf("%s survived the migration", table)
		}
	}
	if linked, err := s.hasColumn(ctx, "purchase_orders", "prq_id"); err != nil || linked {
		t.Fatalf("purchase_orders.prq_id still present (err=%v)", err)
	}

	// The order, its line and its status all survive the table rebuild.
	orders, err := s.List(ctx, "purchase_orders", ListOptions{Sort: "por_id", Limit: 10})
	if err != nil {
		t.Fatalf("list purchase orders: %v", err)
	}
	if len(orders.Rows) != 1 || orders.Rows[0]["por_doc_number"] != "OLD-PO" || orders.Rows[0]["por_status"] != "issued" {
		t.Fatalf("purchase order not carried over: %+v", orders.Rows)
	}
	lines, err := s.List(ctx, "po_components", ListOptions{Sort: "poc_id", Limit: 10})
	if err != nil {
		t.Fatalf("list po lines: %v", err)
	}
	if len(lines.Rows) != 1 {
		t.Fatalf("po line lost in rebuild: %+v", lines.Rows)
	}

	// Approval rules and steps that routed requisitions are cleared with them.
	for _, table := range []string{"approval_rules", "approvals"} {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s kept %d requisition rows", table, count)
		}
	}

	// A fresh write still works after the rebuild.
	if _, err := s.Insert(WithActor(ctx, 1), "purchase_orders", map[string]any{
		"por_doc_number": "NEW-PO", "por_status": "draft", "por_payment_status": "unpaid",
	}); err != nil {
		t.Fatalf("insert after rebuild: %v", err)
	}
}
