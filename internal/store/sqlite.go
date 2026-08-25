package store

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"stockit/internal/auth"
)

type Store struct {
	db     *sql.DB
	tables map[string]TableDef
}

type ListOptions struct {
	Sort   string
	Desc   bool
	Limit  int
	Offset int
	Filter map[string]any
	// From and To bound a column inclusively; both are keyed by column name and
	// are the server-side replacement for date-range filtering in client apps.
	From map[string]any
	To   map[string]any
	// Search matches a case-insensitive substring in any of SearchColumns.
	Search        string
	SearchColumns []string
}

type ListResult struct {
	Rows    []map[string]any
	HasMore bool
}

type Option struct {
	Value string
	Label string
}

type UserRecord struct {
	ID           int64
	LoginName    string
	PasswordHash string
	Role         string
}

func Open(ctx context.Context, dbPath string) (*Store, error) {
	resolvedPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve db path: %w", err)
	}

	dbDir := filepath.Dir(resolvedPath)
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", resolvedPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{
		db:     db,
		tables: AllTables(),
	}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Table(name string) (TableDef, bool) {
	table, ok := s.tables[name]
	return table, ok
}

func (s *Store) TablesForRole(role string) []TableDef {
	tables := make([]TableDef, 0, len(s.tables))
	for _, table := range s.tables {
		if table.CanRead(role) && !table.IsSubtable() {
			tables = append(tables, table)
		}
	}
	slices.SortFunc(tables, func(a, b TableDef) int {
		return strings.Compare(a.Label, b.Label)
	})
	return tables
}

func (s *Store) AuthenticateUser(ctx context.Context, loginName string) (UserRecord, error) {
	row := s.db.QueryRowContext(
		ctx,
		`SELECT usr_id, usr_login_name, usr_password, usr_role FROM users WHERE usr_login_name = ?`,
		loginName,
	)

	var user UserRecord
	if err := row.Scan(&user.ID, &user.LoginName, &user.PasswordHash, &user.Role); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserRecord{}, err
		}
		return UserRecord{}, fmt.Errorf("scan user: %w", err)
	}
	return user, nil
}

func (s *Store) List(ctx context.Context, tableName string, opts ListOptions) (ListResult, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return ListResult{}, fmt.Errorf("unknown table %q", tableName)
	}

	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	sortColumn := table.SortColumn(opts.Sort)
	direction := "ASC"
	if opts.Desc {
		direction = "DESC"
	}

	filterColumns := make([]string, 0, len(opts.Filter))
	for column := range opts.Filter {
		filterColumns = append(filterColumns, column)
	}
	slices.Sort(filterColumns)

	whereClauses := make([]string, 0, len(opts.Filter)+len(opts.From)+len(opts.To)+1)
	args := make([]any, 0, len(opts.Filter)+len(opts.From)+len(opts.To)+len(opts.SearchColumns)+2)
	for _, column := range filterColumns {
		value := opts.Filter[column]
		field, ok := table.Field(column)
		if !ok {
			return ListResult{}, fmt.Errorf("unknown filter column %q for table %q", column, tableName)
		}
		whereClauses = append(whereClauses, fmt.Sprintf("%s = ?", quoteIdent(field.Column)))
		args = append(args, value)
	}
	for _, bound := range []struct {
		values   map[string]any
		operator string
	}{{opts.From, ">="}, {opts.To, "<="}} {
		columns := make([]string, 0, len(bound.values))
		for column := range bound.values {
			columns = append(columns, column)
		}
		slices.Sort(columns)
		for _, column := range columns {
			field, ok := table.Field(column)
			if !ok {
				return ListResult{}, fmt.Errorf("unknown filter column %q for table %q", column, tableName)
			}
			whereClauses = append(whereClauses, fmt.Sprintf("%s %s ?", quoteIdent(field.Column), bound.operator))
			args = append(args, bound.values[column])
		}
	}
	if search := strings.TrimSpace(opts.Search); search != "" && len(opts.SearchColumns) > 0 {
		matches := make([]string, 0, len(opts.SearchColumns))
		for _, column := range opts.SearchColumns {
			field, ok := table.Field(column)
			if !ok {
				return ListResult{}, fmt.Errorf("unknown search column %q for table %q", column, tableName)
			}
			matches = append(matches, fmt.Sprintf(`%s LIKE ? ESCAPE '\'`, quoteIdent(field.Column)))
			args = append(args, "%"+escapeLike(search)+"%")
		}
		whereClauses = append(whereClauses, "("+strings.Join(matches, " OR ")+")")
	}

	whereSQL := ""
	if len(whereClauses) > 0 {
		whereSQL = " WHERE " + strings.Join(whereClauses, " AND ")
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s%s ORDER BY %s %s LIMIT ? OFFSET ?`,
		joinQuoted(selectColumns(table)),
		quoteIdent(table.Name),
		whereSQL,
		quoteIdent(sortColumn),
		direction,
	)
	args = append(args, limit+1, opts.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return ListResult{}, fmt.Errorf("list rows: %w", err)
	}
	defer rows.Close()

	records, err := scanRows(rows)
	if err != nil {
		return ListResult{}, err
	}

	result := ListResult{Rows: records}
	if len(records) > limit {
		result.HasMore = true
		result.Rows = records[:limit]
	}
	return result, nil
}

// escapeLike neutralises the LIKE wildcards so a user search for "50%" does not
// turn into a match-anything pattern.
func escapeLike(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)
	return replacer.Replace(value)
}

func (s *Store) Get(ctx context.Context, tableName string, id string) (map[string]any, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return nil, fmt.Errorf("unknown table %q", tableName)
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s WHERE %s = ?`,
		joinQuoted(selectColumns(table)),
		quoteIdent(table.Name),
		quoteIdent(table.PrimaryKey),
	)
	rows, err := s.db.QueryContext(ctx, query, id)
	if err != nil {
		return nil, fmt.Errorf("get row: %w", err)
	}
	defer rows.Close()

	records, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, sql.ErrNoRows
	}
	return records[0], nil
}

func (s *Store) Insert(ctx context.Context, tableName string, values map[string]any) (int64, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return 0, fmt.Errorf("unknown table %q", tableName)
	}
	if err := guardDecidedStatus(tableName, values); err != nil {
		return 0, err
	}

	columns := table.InsertableColumns(values)
	if len(columns) == 0 {
		query := fmt.Sprintf(`INSERT INTO %s DEFAULT VALUES`, quoteIdent(table.Name))
		result, err := s.db.ExecContext(ctx, query)
		if err != nil {
			return 0, fmt.Errorf("insert default row: %w", err)
		}
		return result.LastInsertId()
	}

	args := make([]any, 0, len(columns))
	placeholders := make([]string, 0, len(columns))
	for _, column := range columns {
		args = append(args, values[column])
		placeholders = append(placeholders, "?")
	}

	query := fmt.Sprintf(
		`INSERT INTO %s (%s) VALUES (%s)`,
		quoteIdent(table.Name),
		joinQuoted(columns),
		strings.Join(placeholders, ", "),
	)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("insert row: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, s.afterWrite(ctx, table.Name, strconv.FormatInt(id, 10), poStatusSnapshot{})
}

// guardDecidedStatus keeps the signing limit meaningful: approved and rejected
// are reachable only through DecidePurchaseOrder, which checks the deciding
// user's limit. Without this a user could set them straight through the table
// API and walk around their own authority.
func guardDecidedStatus(tableName string, values map[string]any) error {
	if tableName != "purchase_orders" {
		return nil
	}
	status, ok := values["por_status"].(string)
	if !ok || !slices.Contains(decidedStatuses, status) {
		return nil
	}
	return fmt.Errorf("%w: %q is set by approving or rejecting the purchase order, not by writing the column", ErrWorkflow, status)
}

// afterWrite keeps the purchase-order audit trail and the derived receipt status
// in step with whatever a generic table write just changed. It is called from
// Insert/Update/Delete so every surface (web UI, REST API, MCP, CSV import)
// records the same history without repeating itself.
func (s *Store) afterWrite(ctx context.Context, tableName, id string, before poStatusSnapshot) error {
	switch tableName {
	case "purchase_orders":
		return s.recordPOStatusChange(ctx, id, before, "")
	case "po_components":
		return s.syncPOReceiptStatus(ctx, s.componentParentID(ctx, id))
	}
	return nil
}

func (s *Store) Update(ctx context.Context, tableName string, id string, values map[string]any) error {
	table, ok := s.Table(tableName)
	if !ok {
		return fmt.Errorf("unknown table %q", tableName)
	}
	if err := guardDecidedStatus(tableName, values); err != nil {
		return err
	}

	columns := table.InsertableColumns(values)
	if len(columns) == 0 {
		return nil
	}

	assignments := make([]string, 0, len(columns))
	args := make([]any, 0, len(columns)+1)
	for _, column := range columns {
		assignments = append(assignments, fmt.Sprintf("%s = ?", quoteIdent(column)))
		args = append(args, values[column])
	}
	args = append(args, id)

	var before poStatusSnapshot
	if table.Name == "purchase_orders" {
		before, _ = s.purchaseOrderSnapshot(ctx, id)
	}

	query := fmt.Sprintf(
		`UPDATE %s SET %s WHERE %s = ?`,
		quoteIdent(table.Name),
		strings.Join(assignments, ", "),
		quoteIdent(table.PrimaryKey),
	)
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update row: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return s.afterWrite(ctx, table.Name, id, before)
}

func (s *Store) Delete(ctx context.Context, tableName string, id string) error {
	table, ok := s.Table(tableName)
	if !ok {
		return fmt.Errorf("unknown table %q", tableName)
	}

	parentPO := ""
	if table.Name == "po_components" {
		parentPO = s.componentParentID(ctx, id)
	}

	query := fmt.Sprintf(`DELETE FROM %s WHERE %s = ?`, quoteIdent(table.Name), quoteIdent(table.PrimaryKey))
	result, err := s.db.ExecContext(ctx, query, id)
	if err != nil {
		return fmt.Errorf("delete row: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err == nil && rowsAffected == 0 {
		return sql.ErrNoRows
	}
	return s.syncPOReceiptStatus(ctx, parentPO)
}

func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE usr_role = 'admin'`)
	var count int
	if err := row.Scan(&count); err != nil {
		return 0, fmt.Errorf("count admins: %w", err)
	}
	return count, nil
}

func (s *Store) ReferenceOptions(ctx context.Context, tableName string) ([]Option, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return nil, fmt.Errorf("unknown reference table %q", tableName)
	}

	columns := table.ReferenceColumns()
	if len(columns) == 0 {
		columns = []string{table.PrimaryKey}
	}

	query := fmt.Sprintf(
		`SELECT %s FROM %s ORDER BY %s ASC`,
		joinQuoted(columns),
		quoteIdent(table.Name),
		quoteIdent(table.TitleColumn),
	)
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list reference options: %w", err)
	}
	defer rows.Close()

	options := []Option{{Value: "", Label: ""}}
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for index := range values {
			dest[index] = &values[index]
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan reference option: %w", err)
		}

		parts := make([]string, 0, len(columns))
		for index, column := range columns {
			field, ok := table.Field(column)
			if !ok {
				continue
			}
			label := DisplayValue(field, normalizeValue(values[index]))
			label = strings.TrimSpace(label)
			if label == "" {
				continue
			}
			parts = append(parts, label)
		}

		value := fmt.Sprint(normalizeValue(values[0]))
		label := strings.Join(parts, " | ")
		if label == "" {
			label = value
		}

		options = append(options, Option{
			Value: value,
			Label: label,
		})
	}
	return options, rows.Err()
}

// ImportOptions parameterizes CSV import so higher layers can inject app-level
// concerns (value transforms, business-rule validation) without the store
// knowing about them.
type ImportOptions struct {
	// Transform converts a raw CSV cell into the stored Go value (required).
	Transform func(Field, string) (any, error)
	// BeforeInsert, if set, receives the assembled row values and may reject
	// the row with an error before it reaches the database.
	BeforeInsert func(values map[string]any) error
}

func (s *Store) ImportCSV(ctx context.Context, tableName string, reader io.Reader, opts ImportOptions) (int, error) {
	table, ok := s.Table(tableName)
	if !ok {
		return 0, fmt.Errorf("unknown table %q", tableName)
	}
	if opts.Transform == nil {
		return 0, fmt.Errorf("ImportOptions.Transform is required")
	}

	csvReader := csv.NewReader(reader)
	csvReader.TrimLeadingSpace = true

	headers, err := csvReader.Read()
	if err != nil {
		return 0, fmt.Errorf("read csv header: %w", err)
	}

	headerMap := make(map[int]Field)
	for index, header := range headers {
		normalized := NormalizeCSVHeader(header)
		for _, field := range table.EditableFields() {
			if NormalizeCSVHeader(field.Column) == normalized || NormalizeCSVHeader(field.Label) == normalized {
				headerMap[index] = field
				break
			}
		}
	}

	inserted := 0
	for {
		record, err := csvReader.Read()
		if errors.Is(err, io.EOF) {
			return inserted, nil
		}
		if err != nil {
			return inserted, fmt.Errorf("read csv row: %w", err)
		}

		values := make(map[string]any)
		for index, rawValue := range record {
			field, ok := headerMap[index]
			if !ok {
				continue
			}
			parsedValue, err := opts.Transform(field, rawValue)
			if err != nil {
				return inserted, fmt.Errorf("parse csv field %s: %w", field.Column, err)
			}
			if parsedValue != nil {
				values[field.Column] = parsedValue
			}
		}

		if opts.BeforeInsert != nil {
			if err := opts.BeforeInsert(values); err != nil {
				return inserted, err
			}
		}

		if _, err := s.Insert(ctx, tableName, values); err != nil {
			return inserted, err
		}
		inserted++
	}
}

func (s *Store) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA foreign_keys = ON;`,
		`PRAGMA journal_mode = WAL;`,
		`CREATE TABLE IF NOT EXISTS users (
			usr_id INTEGER PRIMARY KEY AUTOINCREMENT,
			usr_login_name TEXT NOT NULL UNIQUE,
			usr_password TEXT NOT NULL,
			usr_role TEXT NOT NULL,
			usr_approval_limit_minor INTEGER,
			usr_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS customers (
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
			cus_note TEXT,
			usr_id INTEGER,
			cus_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS suppliers (
			sup_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_code TEXT,
			sup_name_en TEXT NOT NULL,
			sup_name_zh TEXT,
			sup_type TEXT,
			sup_contact_name TEXT,
			sup_contact_phone TEXT,
			sup_contact_email TEXT,
			sup_contact_messanger TEXT,
			sup_fax TEXT,
			sup_address_en TEXT,
			sup_address_zh TEXT,
			sup_factory_adress_zh TEXT,
			sup_website TEXT,
			sup_catalogue_url TEXT,
			sup_bank_name TEXT,
			sup_bank_account TEXT,
			sup_vat_number TEXT,
			sup_certificates TEXT,
			sup_note TEXT,
			usr_id INTEGER,
			sup_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS locations (
			loc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			loc_name TEXT NOT NULL,
			loc_address_en TEXT,
			loc_address_zh TEXT,
			loc_zone TEXT,
			loc_note TEXT,
			usr_id INTEGER,
			loc_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS items (
			itm_id INTEGER PRIMARY KEY AUTOINCREMENT,
			itm_sku TEXT NOT NULL,
			itm_model TEXT,
			itm_description TEXT,
			itm_value REAL,
			itm_last_cost REAL,
			itm_avg_cost REAL,
			itm_type TEXT,
			itm_pic BLOB,
			itm_measure_unit TEXT,
			itm_note TEXT,
			usr_id INTEGER,
			itm_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS boms (
			bom_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bom_doc_number TEXT NOT NULL,
			bom_doc_date TEXT,
			itm_id INTEGER,
			bom_note TEXT,
			usr_id INTEGER,
			bom_status TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS bom_components (
			boc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bom_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			boc_qty REAL,
			boc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (bom_id) REFERENCES boms (bom_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS purchase_orders (
			por_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			por_doc_number TEXT NOT NULL,
			por_doc_date TEXT,
			itm_id INTEGER,
			por_ship_date TEXT,
			por_paid_date TEXT,
			usr_id INTEGER,
			por_status TEXT,
			por_payment_status TEXT,
			por_currency TEXT,
			por_total_minor INTEGER,
			por_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS quotes (
			qot_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			qot_doc_number TEXT NOT NULL,
			qot_doc_date TEXT,
			itm_id INTEGER,
			usr_id INTEGER,
			qot_status TEXT,
			qot_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS quote_components (
			qoc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			qot_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			qot_moq REAL,
			qot_qty REAL,
			qot_price REAL,
			qot_currency TEXT,
			qot_lead_time TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (qot_id) REFERENCES quotes (qot_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS po_components (
			poc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			por_id INTEGER NOT NULL,
			itm_id INTEGER NOT NULL,
			poc_qty REAL,
			poc_price REAL,
			poc_currency TEXT,
			poc_shipped_date TEXT,
			poc_delivered_date TEXT,
			poc_delivered_qty REAL,
			poc_received_date TEXT,
			poc_received_qty REAL,
			poc_iqc_date TEXT,
			poc_iqc_package TEXT,
			poc_iqc_qty_inspected REAL,
			poc_iqc_qty_accepted REAL,
			poc_iqc_qty_rejected REAL,
			poc_iqc_person INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (por_id) REFERENCES purchase_orders (por_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (poc_iqc_person) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS sales_orders (
			sor_id INTEGER PRIMARY KEY AUTOINCREMENT,
			cus_id INTEGER,
			sor_doc_number TEXT NOT NULL,
			sor_doc_date TEXT,
			sor_ship_date TEXT,
			sor_paid_date TEXT,
			usr_id INTEGER,
			sor_status TEXT,
			sor_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (cus_id) REFERENCES customers (cus_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS manufacturing_orders (
			mfo_id INTEGER PRIMARY KEY AUTOINCREMENT,
			mfo_doc_number TEXT NOT NULL,
			mfo_doc_date TEXT,
			mfo_target_date TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS mfo_components (
			mfc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			mfo_id INTEGER NOT NULL,
			itm_id INTEGER,
			bom_id INTEGER,
			mfc_qty REAL,
			sor_id INTEGER,
			mfc_qc_date TEXT,
			mfc_fqc_date TEXT,
			mfc_pack_date TEXT,
			mfc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (mfo_id) REFERENCES manufacturing_orders (mfo_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (bom_id) REFERENCES boms (bom_id),
			FOREIGN KEY (sor_id) REFERENCES sales_orders (sor_id)
		);`,
		`CREATE TABLE IF NOT EXISTS sales_order_components (
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
			soc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sor_id) REFERENCES sales_orders (sor_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS invoices (
			inv_id INTEGER PRIMARY KEY AUTOINCREMENT,
			inv_doc_number TEXT NOT NULL,
			inv_doc_date TEXT,
			sup_id INTEGER,
			cus_id INTEGER,
			sor_id INTEGER,
			inv_shipped_by TEXT,
			usr_id INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (cus_id) REFERENCES customers (cus_id),
			FOREIGN KEY (sor_id) REFERENCES sales_orders (sor_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS invoice_components (
			ivc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			inv_id INTEGER NOT NULL,
			itm_id INTEGER,
			ivc_qty REAL,
			ivc_price REAL,
			ivc_currency TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (inv_id) REFERENCES invoices (inv_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id)
		);`,
		`CREATE TABLE IF NOT EXISTS adjustments (
			adj_id INTEGER PRIMARY KEY AUTOINCREMENT,
			adj_doc_number TEXT NOT NULL,
			adj_doc_date TEXT,
			adj_reason TEXT,
			usr_id INTEGER,
			adj_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS adjustment_components (
			adc_id INTEGER PRIMARY KEY AUTOINCREMENT,
			adj_id INTEGER NOT NULL,
			itm_id INTEGER,
			loc_id INTEGER,
			adc_qty REAL,
			adc_price REAL,
			adc_currency TEXT,
			adc_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (adj_id) REFERENCES adjustments (adj_id) ON DELETE CASCADE,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (loc_id) REFERENCES locations (loc_id)
		);`,
		`CREATE TABLE IF NOT EXISTS stock_moves (
			stm_id INTEGER PRIMARY KEY AUTOINCREMENT,
			stm_doc_number TEXT NOT NULL,
			stm_date TEXT,
			itm_id INTEGER,
			stm_src_loc_id INTEGER,
			stm_dst_loc_id INTEGER,
			stm_qty REAL,
			por_id INTEGER,
			sor_id INTEGER,
			mfo_id INTEGER,
			adj_id INTEGER,
			usr_id INTEGER,
			stm_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (stm_src_loc_id) REFERENCES locations (loc_id),
			FOREIGN KEY (stm_dst_loc_id) REFERENCES locations (loc_id),
			FOREIGN KEY (por_id) REFERENCES purchase_orders (por_id),
			FOREIGN KEY (sor_id) REFERENCES sales_orders (sor_id),
			FOREIGN KEY (mfo_id) REFERENCES manufacturing_orders (mfo_id),
			FOREIGN KEY (adj_id) REFERENCES adjustments (adj_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS bank_accounts (
			bnk_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bnk_name TEXT NOT NULL,
			bnk_institution TEXT,
			bnk_currency TEXT NOT NULL,
			bnk_account_reference TEXT,
			bnk_status TEXT,
			bnk_note TEXT,
			usr_id INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS designation_codes (
			dsg_id INTEGER PRIMARY KEY AUTOINCREMENT,
			dsg_code TEXT NOT NULL UNIQUE,
			dsg_name TEXT NOT NULL,
			dsg_direction TEXT NOT NULL,
			dsg_status TEXT,
			dsg_note TEXT,
			usr_id INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS financial_obligations (
			fob_id INTEGER PRIMARY KEY AUTOINCREMENT,
			fob_type TEXT NOT NULL,
			fob_source_type TEXT NOT NULL,
			fob_source_id INTEGER,
			fob_label TEXT NOT NULL,
			fob_due_date TEXT NOT NULL,
			fob_amount_minor INTEGER NOT NULL,
			fob_currency TEXT NOT NULL,
			fob_status TEXT NOT NULL,
			fob_counterparty TEXT,
			fob_note TEXT,
			usr_id INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS po_status_history (
			psh_id INTEGER PRIMARY KEY AUTOINCREMENT,
			por_id INTEGER NOT NULL,
			psh_previous_status TEXT,
			psh_status TEXT NOT NULL,
			psh_payment_status TEXT,
			usr_id INTEGER,
			psh_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (por_id) REFERENCES purchase_orders (por_id) ON DELETE CASCADE,
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		`CREATE TABLE IF NOT EXISTS bank_transactions (
			btx_id INTEGER PRIMARY KEY AUTOINCREMENT,
			bnk_id INTEGER NOT NULL,
			btx_date TEXT NOT NULL,
			btx_amount_minor INTEGER NOT NULL,
			btx_designation_code TEXT NOT NULL,
			btx_description TEXT,
			btx_counterparty TEXT,
			btx_external_reference TEXT,
			fob_id INTEGER,
			btx_reconciliation_status TEXT NOT NULL,
			btx_note TEXT,
			usr_id INTEGER,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (bnk_id) REFERENCES bank_accounts (bnk_id),
			FOREIGN KEY (btx_designation_code) REFERENCES designation_codes (dsg_code),
			FOREIGN KEY (fob_id) REFERENCES financial_obligations (fob_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
	}

	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("run schema statement: %w", err)
		}
	}

	if err := s.dropRetiredApprovalTables(ctx); err != nil {
		return err
	}

	if err := s.ensureColumn(ctx, "boms", "bom_doc_date", "TEXT"); err != nil {
		return err
	}
	for _, migration := range []struct {
		table  string
		column string
		def    string
	}{
		{table: "users", column: "usr_note", def: "TEXT"},
		{table: "customers", column: "cus_note", def: "TEXT"},
		{table: "locations", column: "loc_note", def: "TEXT"},
		{table: "items", column: "itm_last_cost", def: "REAL"},
		{table: "items", column: "itm_avg_cost", def: "REAL"},
		{table: "items", column: "itm_note", def: "TEXT"},
		{table: "quotes", column: "qot_note", def: "TEXT"},
		{table: "sales_orders", column: "sor_note", def: "TEXT"},
		{table: "sales_order_components", column: "soc_note", def: "TEXT"},
		{table: "po_components", column: "poc_iqc_date", def: "TEXT"},
		{table: "po_components", column: "poc_iqc_package", def: "TEXT"},
		{table: "po_components", column: "poc_iqc_qty_inspected", def: "REAL"},
		{table: "po_components", column: "poc_iqc_qty_accepted", def: "REAL"},
		{table: "po_components", column: "poc_iqc_qty_rejected", def: "REAL"},
		{table: "po_components", column: "poc_iqc_person", def: "INTEGER"},
		{table: "purchase_orders", column: "por_payment_status", def: "TEXT"},
		{table: "purchase_orders", column: "por_currency", def: "TEXT"},
		{table: "purchase_orders", column: "por_total_minor", def: "INTEGER"},
		{table: "users", column: "usr_approval_limit_minor", def: "INTEGER"},
	} {
		if err := s.ensureColumn(ctx, migration.table, migration.column, migration.def); err != nil {
			return err
		}
	}

	if err := s.renameStatusValue(ctx, "Absolete", "Obsolete"); err != nil {
		return err
	}

	if err := s.migratePOStatuses(ctx); err != nil {
		return err
	}

	return s.seedDefaults(ctx)
}

// renameStatusValue rewrites a legacy status value across every status column
// that uses the shared statusOptions enum. Kept as a one-shot data migration so
// older databases do not carry the misspelled "Absolete" forward.
func (s *Store) renameStatusValue(ctx context.Context, oldValue, newValue string) error {
	columns := []struct {
		table  string
		column string
	}{
		{table: "customers", column: "cus_status"},
		{table: "suppliers", column: "sup_status"},
		{table: "locations", column: "loc_status"},
		{table: "items", column: "itm_status"},
		{table: "boms", column: "bom_status"},
	}
	for _, c := range columns {
		query := fmt.Sprintf(
			`UPDATE %s SET %s = ? WHERE %s = ?`,
			quoteIdent(c.table),
			quoteIdent(c.column),
			quoteIdent(c.column),
		)
		if _, err := s.db.ExecContext(ctx, query, newValue, oldValue); err != nil {
			return fmt.Errorf("rename status value in %s.%s: %w", c.table, c.column, err)
		}
	}
	return nil
}

// dropRetiredApprovalTables removes the purchase requisition and approval-rule
// tables and the
// purchase_orders.prq_id link from databases written before requisitions were
// retired. purchase_orders has to be rebuilt because SQLite refuses to drop a
// column that a foreign key clause names, and the parent table cannot be
// dropped while that column still holds matching values.
func (s *Store) dropRetiredApprovalTables(ctx context.Context) error {
	linked, err := s.hasColumn(ctx, "purchase_orders", "prq_id")
	if err != nil {
		return err
	}
	if linked {
		if err := s.rebuildPurchaseOrdersWithoutRequisitionLink(ctx); err != nil {
			return err
		}
	}
	// approval_rules and approvals went with the amount-routing engine: approval
	// authority is now a per-user limit on users.usr_approval_limit_minor, and a
	// single decision is recorded in po_status_history like any status change.
	for _, table := range []string{"prq_components", "purchase_requisitions", "approvals", "approval_rules"} {
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, quoteIdent(table))); err != nil {
			return fmt.Errorf("drop %s: %w", table, err)
		}
	}
	return nil
}

// rebuildPurchaseOrdersWithoutRequisitionLink copies purchase_orders into a
// table built from the current schema, carrying over every column the two share.
func (s *Store) rebuildPurchaseOrdersWithoutRequisitionLink(ctx context.Context) error {
	table, ok := s.Table("purchase_orders")
	if !ok {
		return fmt.Errorf("purchase_orders is not a known table")
	}
	shared := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		exists, err := s.hasColumn(ctx, "purchase_orders", field.Column)
		if err != nil {
			return err
		}
		if exists {
			shared = append(shared, field.Column)
		}
	}

	// Foreign keys stay off for the swap: po_components and stock_moves point at
	// purchase_orders and must survive the drop, and the rename reattaches them.
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return fmt.Errorf("disable foreign keys: %w", err)
	}
	defer func() { _, _ = s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`) }()

	for _, statement := range []string{
		`CREATE TABLE purchase_orders_rebuilt (
			por_id INTEGER PRIMARY KEY AUTOINCREMENT,
			sup_id INTEGER,
			por_doc_number TEXT NOT NULL,
			por_doc_date TEXT,
			itm_id INTEGER,
			por_ship_date TEXT,
			por_paid_date TEXT,
			usr_id INTEGER,
			por_status TEXT,
			por_payment_status TEXT,
			por_currency TEXT,
			por_total_minor INTEGER,
			por_note TEXT,
			created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (sup_id) REFERENCES suppliers (sup_id),
			FOREIGN KEY (itm_id) REFERENCES items (itm_id),
			FOREIGN KEY (usr_id) REFERENCES users (usr_id)
		);`,
		fmt.Sprintf(`INSERT INTO purchase_orders_rebuilt (%s) SELECT %s FROM purchase_orders`,
			joinQuoted(shared), joinQuoted(shared)),
		`DROP TABLE purchase_orders`,
		`ALTER TABLE purchase_orders_rebuilt RENAME TO purchase_orders`,
	} {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("rebuild purchase_orders: %w", err)
		}
	}
	return nil
}

// migratePOStatuses rewrites the pre-lifecycle purchase-order statuses onto the
// current set and backfills the payment tag, so databases written before the
// lifecycle statuses landed still validate against the enum.
func (s *Store) migratePOStatuses(ctx context.Context) error {
	// "paid" carried both meanings; it becomes a closed order tagged paid.
	if _, err := s.db.ExecContext(ctx, `
		UPDATE purchase_orders SET por_payment_status = 'paid' WHERE por_status = 'paid'`); err != nil {
		return fmt.Errorf("backfill purchase order payment status: %w", err)
	}
	for _, rename := range []struct{ from, to string }{
		{from: "sent", to: "issued"},
		{from: "prepared", to: "confirmed"},
		{from: "shipped", to: "confirmed"},
		{from: "delivered", to: "received"},
		{from: "paid", to: "closed"},
		{from: "complete", to: "closed"},
		{from: "inactive", to: "cancelled"},
	} {
		if _, err := s.db.ExecContext(ctx,
			`UPDATE purchase_orders SET por_status = ? WHERE por_status = ?`, rename.to, rename.from); err != nil {
			return fmt.Errorf("migrate purchase order status %q: %w", rename.from, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE purchase_orders SET por_payment_status = 'unpaid'
		WHERE por_payment_status IS NULL OR TRIM(por_payment_status) = ''`); err != nil {
		return fmt.Errorf("default purchase order payment status: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, tableName, columnName, columnDef string) error {
	exists, err := s.hasColumn(ctx, tableName, columnName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	statement := fmt.Sprintf(
		`ALTER TABLE %s ADD COLUMN %s %s`,
		quoteIdent(tableName),
		quoteIdent(columnName),
		columnDef,
	)
	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("add column %s.%s: %w", tableName, columnName, err)
	}
	return nil
}

func (s *Store) hasColumn(ctx context.Context, tableName, columnName string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, quoteIdent(tableName)))
	if err != nil {
		return false, fmt.Errorf("inspect table %s: %w", tableName, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return false, fmt.Errorf("scan table info for %s: %w", tableName, err)
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("iterate table info for %s: %w", tableName, err)
	}
	return false, nil
}

func (s *Store) seedDefaults(ctx context.Context) error {
	if err := s.seedDesignationCodes(ctx); err != nil {
		return err
	}
	// An admin is the superuser, so it carries approval authority above any
	// realistic order. Every other user starts with none until someone sets a
	// limit; an empty limit means "may not approve".
	if _, err := s.db.ExecContext(ctx, `
		UPDATE users SET usr_approval_limit_minor = ?
		WHERE usr_role = 'admin' AND usr_approval_limit_minor IS NULL`, AdminApprovalLimitMinor); err != nil {
		return fmt.Errorf("backfill admin approval limit: %w", err)
	}

	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`)
	var count int
	if err := row.Scan(&count); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	if count > 0 {
		return nil
	}

	defaultUsers := []struct {
		Login string
		Pass  string
		Role  string
		Limit any
	}{
		{Login: "admin", Pass: "admin", Role: "admin", Limit: AdminApprovalLimitMinor},
		{Login: "user", Pass: "user", Role: "user"},
		{Login: "guest", Pass: "guest", Role: "guest"},
	}

	for _, user := range defaultUsers {
		hash, err := auth.HashPassword(user.Pass)
		if err != nil {
			return fmt.Errorf("hash password for %s: %w", user.Login, err)
		}
		if _, err := s.db.ExecContext(
			ctx,
			`INSERT INTO users (usr_login_name, usr_password, usr_role, usr_approval_limit_minor) VALUES (?, ?, ?, ?)`,
			user.Login,
			hash,
			user.Role,
			user.Limit,
		); err != nil {
			return fmt.Errorf("seed default user %s: %w", user.Login, err)
		}
	}

	return nil
}

func (s *Store) seedDesignationCodes(ctx context.Context) error {
	names := map[string]string{
		"INVENTORY": "Inventory", "COGS": "Cost of Goods Sold", "GOODS": "Goods",
		"SERVICES": "Services", "SHIPPING": "Shipping", "PAYROLL": "Payroll",
		"TAX": "Tax", "BANK_FEE": "Bank Fee", "RENT": "Rent", "UTILITIES": "Utilities",
		"SALES_REVENUE": "Sales Revenue", "CUSTOMER_REFUND": "Customer Refund",
		"TRANSFER": "Internal Transfer", "OTHER": "Other",
	}
	for _, code := range designationDefaults {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO designation_codes (dsg_code, dsg_name, dsg_direction, dsg_status) VALUES (?, ?, ?, 'Active')`,
			code, names[code], "either"); err != nil {
			return fmt.Errorf("seed designation code %s: %w", code, err)
		}
	}
	return nil
}

func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("read row columns: %w", err)
	}

	records := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for index := range values {
			dest[index] = &values[index]
		}

		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}

		record := make(map[string]any, len(columns))
		for index, column := range columns {
			record[column] = normalizeValue(values[index])
		}
		records = append(records, record)
	}

	return records, rows.Err()
}

func normalizeValue(value any) any {
	switch typed := value.(type) {
	case nil:
		return nil
	case []byte:
		clone := make([]byte, len(typed))
		copy(clone, typed)
		return clone
	default:
		return typed
	}
}

func ParseFieldValue(field Field, rawValue string) (any, error) {
	trimmed := strings.TrimSpace(rawValue)
	if trimmed == "" {
		switch field.Kind {
		case KindText, KindTextarea, KindPassword:
			return "", nil
		default:
			return nil, nil
		}
	}

	switch field.Kind {
	case KindInteger, KindForeign:
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case KindReal:
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, err
		}
		return parsed, nil
	case KindDate:
		parsed, err := time.Parse(time.DateOnly, trimmed)
		if err != nil {
			return nil, err
		}
		return parsed.Format(time.DateOnly), nil
	default:
		return trimmed, nil
	}
}

func quoteIdent(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}

func joinQuoted(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, quoteIdent(value))
	}
	return strings.Join(quoted, ", ")
}

func selectColumns(table TableDef) []string {
	columns := make([]string, 0, len(table.Fields))
	for _, field := range table.Fields {
		if field.Kind == KindPassword {
			continue
		}
		columns = append(columns, field.Column)
	}
	return columns
}
