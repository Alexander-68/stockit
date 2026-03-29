package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenSeedsUsersAndHidesPasswordHashes(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	users, err := s.List(context.Background(), "users", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users.Rows) != 3 {
		t.Fatalf("expected 3 default users, got %d", len(users.Rows))
	}
	for _, row := range users.Rows {
		if _, ok := row["usr_password"]; ok {
			t.Fatalf("password hash should not be listed: %+v", row)
		}
	}
}

func TestImportCSVAndBOMDeleteCascadesComponents(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	imported, err := s.ImportCSV(ctx, "customers", strings.NewReader(""+
		"cus_name_en,cus_phone,cus_status\n"+
		"Import One,1000,Active\n"+
		"Import Two,2000,Hold\n"), ParseFieldValue)
	if err != nil {
		t.Fatalf("import customers csv: %v", err)
	}
	if imported != 2 {
		t.Fatalf("imported rows = %d, want 2", imported)
	}

	customers, err := s.List(ctx, "customers", ListOptions{Limit: 10, Sort: "cus_name_en"})
	if err != nil {
		t.Fatalf("list customers: %v", err)
	}
	if len(customers.Rows) != 2 {
		t.Fatalf("expected 2 imported customers, got %d", len(customers.Rows))
	}

	finalItemID, err := s.Insert(ctx, "items", map[string]any{
		"itm_sku":          "FG-001",
		"itm_model":        "Final",
		"itm_type":         "final",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	if err != nil {
		t.Fatalf("insert final item: %v", err)
	}
	partItemID, err := s.Insert(ctx, "items", map[string]any{
		"itm_sku":          "PT-001",
		"itm_model":        "Part",
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	if err != nil {
		t.Fatalf("insert part item: %v", err)
	}
	bomID, err := s.Insert(ctx, "boms", map[string]any{
		"bom_doc_number": "BOM-001",
		"itm_id":         finalItemID,
		"bom_status":     "Active",
	})
	if err != nil {
		t.Fatalf("insert bom: %v", err)
	}
	if _, err := s.Insert(ctx, "bom_components", map[string]any{
		"bom_id":  bomID,
		"itm_id":  partItemID,
		"boc_qty": 2.0,
	}); err != nil {
		t.Fatalf("insert bom component: %v", err)
	}

	if err := s.Delete(ctx, "boms", strconv.FormatInt(bomID, 10)); err != nil {
		t.Fatalf("delete bom: %v", err)
	}

	components, err := s.List(ctx, "bom_components", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list bom components: %v", err)
	}
	if len(components.Rows) != 0 {
		t.Fatalf("expected bom components to cascade delete, got %+v", components.Rows)
	}

	supplierID, err := s.Insert(ctx, "suppliers", map[string]any{
		"sup_name_en": "Cascade Supplier",
		"sup_status":  "Active",
	})
	if err != nil {
		t.Fatalf("insert supplier: %v", err)
	}

	porID, err := s.Insert(ctx, "purchase_orders", map[string]any{
		"sup_id":         supplierID,
		"por_doc_number": "PO-001",
		"por_doc_date":   "2026-03-26",
		"itm_id":         finalItemID,
		"por_status":     "approved",
		"por_note":       "Cascade purchase order",
		"por_paid_date":  "2026-03-29",
		"por_ship_date":  "2026-03-27",
	})
	if err != nil {
		t.Fatalf("insert purchase order: %v", err)
	}
	if _, err := s.Insert(ctx, "po_components", map[string]any{
		"por_id":             porID,
		"itm_id":             partItemID,
		"poc_qty":            4.0,
		"poc_price":          19.5,
		"poc_currency":       "USD",
		"poc_shipped_date":   "2026-03-28",
		"poc_delivered_date": "2026-03-30",
		"poc_delivered_qty":  4.0,
		"poc_received_date":  "2026-03-31",
		"poc_received_qty":   4.0,
	}); err != nil {
		t.Fatalf("insert po component: %v", err)
	}

	if err := s.Delete(ctx, "purchase_orders", strconv.FormatInt(porID, 10)); err != nil {
		t.Fatalf("delete purchase order: %v", err)
	}

	poComponents, err := s.List(ctx, "po_components", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list po components: %v", err)
	}
	if len(poComponents.Rows) != 0 {
		t.Fatalf("expected po components to cascade delete, got %+v", poComponents.Rows)
	}

	quoteID, err := s.Insert(ctx, "quotes", map[string]any{
		"sup_id":         supplierID,
		"qot_doc_number": "QT-001",
		"qot_doc_date":   "2026-03-26",
		"itm_id":         finalItemID,
		"qot_status":     "active",
	})
	if err != nil {
		t.Fatalf("insert quote: %v", err)
	}
	if _, err := s.Insert(ctx, "quote_components", map[string]any{
		"qot_id":        quoteID,
		"itm_id":        partItemID,
		"qot_moq":       10.0,
		"qot_qty":       25.0,
		"qot_price":     2.75,
		"qot_currency":  "USD",
		"qot_lead_time": "14 days",
	}); err != nil {
		t.Fatalf("insert quote component: %v", err)
	}

	if err := s.Delete(ctx, "quotes", strconv.FormatInt(quoteID, 10)); err != nil {
		t.Fatalf("delete quote: %v", err)
	}

	quoteComponents, err := s.List(ctx, "quote_components", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list quote components: %v", err)
	}
	if len(quoteComponents.Rows) != 0 {
		t.Fatalf("expected quote components to cascade delete, got %+v", quoteComponents.Rows)
	}

	customerID, err := s.Insert(ctx, "customers", map[string]any{
		"cus_name_en": "Cascade Customer",
		"cus_status":  "Active",
	})
	if err != nil {
		t.Fatalf("insert customer: %v", err)
	}

	salesOrderID, err := s.Insert(ctx, "sales_orders", map[string]any{
		"cus_id":         customerID,
		"sor_doc_number": "SO-001",
		"sor_doc_date":   "2026-03-26",
		"sor_ship_date":  "2026-03-28",
		"sor_paid_date":  "2026-03-29",
		"sor_status":     "confirmed",
	})
	if err != nil {
		t.Fatalf("insert sales order: %v", err)
	}
	if _, err := s.Insert(ctx, "sales_order_components", map[string]any{
		"sor_id":              salesOrderID,
		"itm_id":              partItemID,
		"sor_qty":             6.0,
		"sor_price":           5.4,
		"sor_currency":        "EUR",
		"sor_ship_date":       "2026-03-28",
		"sor_shipped_date":    "2026-03-29",
		"sor_shipped_qty":     6.0,
		"sor_shipped_trackno": "TRACK-001",
	}); err != nil {
		t.Fatalf("insert sales order component: %v", err)
	}

	if err := s.Delete(ctx, "sales_orders", strconv.FormatInt(salesOrderID, 10)); err != nil {
		t.Fatalf("delete sales order: %v", err)
	}

	salesOrderComponents, err := s.List(ctx, "sales_order_components", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list sales order components: %v", err)
	}
	if len(salesOrderComponents.Rows) != 0 {
		t.Fatalf("expected sales order components to cascade delete, got %+v", salesOrderComponents.Rows)
	}
}

func TestItemsLastAndAverageCostRoundTrip(t *testing.T) {
	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	itemID, err := s.Insert(ctx, "items", map[string]any{
		"itm_sku":          "COST-001",
		"itm_model":        "Costed Item",
		"itm_value":        25.75,
		"itm_last_cost":    12.5,
		"itm_avg_cost":     11.25,
		"itm_type":         "part",
		"itm_measure_unit": "pcs",
		"itm_status":       "Active",
	})
	if err != nil {
		t.Fatalf("insert item: %v", err)
	}

	row, err := s.Get(ctx, "items", strconv.FormatInt(itemID, 10))
	if err != nil {
		t.Fatalf("get item: %v", err)
	}

	if got := row["itm_last_cost"]; got != 12.5 {
		t.Fatalf("itm_last_cost = %v, want 12.5", got)
	}
	if got := row["itm_avg_cost"]; got != 11.25 {
		t.Fatalf("itm_avg_cost = %v, want 11.25", got)
	}
}

func TestOpenMigratesLegacyBOMSchemaAndAddsPurchaseOrderTables(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "stockit.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite database: %v", err)
	}
	_, err = db.ExecContext(ctx, `CREATE TABLE boms (
		bom_id INTEGER PRIMARY KEY AUTOINCREMENT,
		bom_doc_number TEXT NOT NULL,
		itm_id INTEGER,
		bom_note TEXT,
		usr_id INTEGER,
		bom_status TEXT,
		created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
	);`)
	if err != nil {
		t.Fatalf("create legacy boms table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy sqlite database: %v", err)
	}

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer func() { _ = s.Close() }()

	if _, ok := s.Table("purchase_orders"); !ok {
		t.Fatalf("purchase_orders table metadata missing after open")
	}
	if _, ok := s.Table("po_components"); !ok {
		t.Fatalf("po_components table metadata missing after open")
	}
	if _, ok := s.Table("quotes"); !ok {
		t.Fatalf("quotes table metadata missing after open")
	}
	if _, ok := s.Table("quote_components"); !ok {
		t.Fatalf("quote_components table metadata missing after open")
	}
	if _, ok := s.Table("sales_orders"); !ok {
		t.Fatalf("sales_orders table metadata missing after open")
	}
	if _, ok := s.Table("sales_order_components"); !ok {
		t.Fatalf("sales_order_components table metadata missing after open")
	}

	if _, err := s.Insert(ctx, "boms", map[string]any{
		"bom_doc_number": "BOM-MIG-001",
		"bom_doc_date":   "2026-03-26",
		"bom_status":     "Active",
	}); err != nil {
		t.Fatalf("insert migrated bom with doc date: %v", err)
	}

	boms, err := s.List(ctx, "boms", ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list boms: %v", err)
	}
	if len(boms.Rows) == 0 {
		t.Fatal("expected migrated bom row to be listed")
	}
	if got := boms.Rows[0]["bom_doc_date"]; got != "2026-03-26" {
		t.Fatalf("bom_doc_date = %v, want 2026-03-26", got)
	}
}

func TestOpenMigratesLegacyNoteColumns(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "stockit.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy sqlite database: %v", err)
	}
	for _, statement := range []string{
		`CREATE TABLE users (
			usr_id INTEGER PRIMARY KEY AUTOINCREMENT,
			usr_login_name TEXT NOT NULL UNIQUE,
			usr_password TEXT NOT NULL,
			usr_role TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE customers (
			cus_id INTEGER PRIMARY KEY AUTOINCREMENT,
			cus_name_en TEXT NOT NULL,
			cus_name_zh TEXT,
			cus_address_en TEXT,
			cus_address_zh TEXT,
			cus_phone TEXT,
			cus_ship_address_en TEXT,
			cus_ship_address_zh TEXT,
			cus_contact_name TEXT,
			cust_contact_email TEXT,
			usr_id INTEGER,
			cus_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE locations (
			loc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			loc_name TEXT NOT NULL,
			loc_address_en TEXT,
			loc_address_zh TEXT,
			loc_zone TEXT,
			usr_id INTEGER,
			loc_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE items (
			itm_id INTEGER PRIMARY KEY AUTOINCREMENT,
			itm_sku TEXT NOT NULL,
			itm_model TEXT,
			itm_description TEXT,
			itm_value REAL,
			itm_type TEXT,
			itm_pic BLOB,
			itm_measure_unit TEXT,
			usr_id INTEGER,
			itm_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE quotes (
			qot_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			qot_doc_number TEXT NOT NULL,
			qot_doc_date TEXT,
			itm_id INTEGER,
			usr_id INTEGER,
			qot_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE sales_orders (
			sor_id INTEGER PRIMARY KEY AUTOINCREMENT,
			cus_id INTEGER,
			sor_doc_number TEXT NOT NULL,
			sor_doc_date TEXT,
			sor_ship_date TEXT,
			sor_paid_date TEXT,
			usr_id INTEGER,
			sor_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE sales_order_components (
			soc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sor_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			sor_qty REAL,
			sor_price REAL,
			sor_currency TEXT,
			sor_ship_date TEXT,
			sor_shipped_date TEXT,
			sor_shipped_qty REAL,
			sor_shipped_trackno TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("create legacy table: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close legacy sqlite database: %v", err)
	}

	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer func() { _ = s.Close() }()

	for _, tc := range []struct {
		table  string
		column string
	}{
		{table: "users", column: "usr_note"},
		{table: "customers", column: "cus_note"},
		{table: "locations", column: "loc_note"},
		{table: "items", column: "itm_last_cost"},
		{table: "items", column: "itm_avg_cost"},
		{table: "items", column: "itm_note"},
		{table: "quotes", column: "qot_note"},
		{table: "sales_orders", column: "sor_note"},
		{table: "sales_order_components", column: "soc_note"},
	} {
		ok, err := s.hasColumn(ctx, tc.table, tc.column)
		if err != nil {
			t.Fatalf("hasColumn(%s, %s): %v", tc.table, tc.column, err)
		}
		if !ok {
			t.Fatalf("expected %s.%s to be added during migration", tc.table, tc.column)
		}
	}
}

func TestOpenDoesNotMutateProcessTempEnvironment(t *testing.T) {
	sentinelDir := t.TempDir()
	t.Setenv("TMPDIR", sentinelDir)
	t.Setenv("SQLITE_TMPDIR", sentinelDir)

	s := openTestStore(t)
	defer func() { _ = s.Close() }()

	if got := os.Getenv("TMPDIR"); got != sentinelDir {
		t.Fatalf("TMPDIR = %q, want %q", got, sentinelDir)
	}
	if got := os.Getenv("SQLITE_TMPDIR"); got != sentinelDir {
		t.Fatalf("SQLITE_TMPDIR = %q, want %q", got, sentinelDir)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()

	store, err := Open(context.Background(), filepath.Join(t.TempDir(), "stockit.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	return store
}
